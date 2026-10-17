package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/buildreport"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/matrix"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/fatih/color"
)

// This file holds the execution core shared by the three matrix execution
// entry points:
//
//	fresh build  (runMatrixMode)      — new Run, all combinations
//	resume       (resumeMatrixRun)    — existing Run, incomplete combinations
//	manual retry (matrix retry)       — new linked Run, failed combinations
//
// All three persist the run incrementally (one update per completed
// combination), hold the run lock for the whole session, and translate
// SIGINT/SIGTERM into an interruptible session whose unfinished combinations
// stay pending — that is what makes `--resume` safe.

// matrixSessionObserver receives incremental progress from a matrix
// execution session. SessionStarted fires once, after the run lock is
// acquired and before any combination executes; AttemptStarted/ResultRecorded
// fire per attempt start and final combination result, after they have been
// recorded into the run and persisted. The local CLI passes nil (no
// observer); the remote matrix handlers pass an observer that streams
// MatrixEvents to the backend. It is called from executor worker goroutines,
// so implementations must be safe for concurrent use.
type matrixSessionObserver interface {
	SessionStarted(run *matrix.Run)
	AttemptStarted(run *matrix.Run, c matrix.Combination, attempt int)
	ResultRecorded(run *matrix.Run, res matrix.Result)
}

// withMatrixSignals runs fn with SIGINT/SIGTERM wired to cancel its context —
// the local-CLI interruption path. The first signal cancels the session (no
// new combinations start, in-flight builds are killed, their combinations stay
// pending) and restores the default signal disposition so a second signal
// terminates the process.
func withMatrixSignals(fn func(ctx context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sessionDone := make(chan struct{})
	defer close(sessionDone)
	go func() {
		select {
		case <-sigCh:
			cancel()
			signal.Stop(sigCh)
			signal.Reset(os.Interrupt, syscall.SIGTERM)
		case <-sessionDone:
		}
	}()
	fn(ctx)
	signal.Stop(sigCh)
}

// startMatrixSession is the local CLI entry point for a FRESH build or a manual
// retry: signal handling around startMatrixSessionContext. The run must already
// be persisted in its pre-execution state (via SaveRun). Resume has its own
// entry point (resumeMatrixSession) because it must mutate the run under the
// run lock.
func startMatrixSession(run *matrix.Run, combos []matrix.Combination, extraArgs []string, debug bool) (executed []matrix.Result, interrupted bool, err error) {
	withMatrixSignals(func(ctx context.Context) {
		executed, interrupted, err = startMatrixSessionContext(ctx, run, combos, extraArgs, debug, nil)
	})
	return executed, interrupted, err
}

// startMatrixSessionContext is the FRESH-build / manual-retry entry point into
// the shared execution core: it acquires the run's execution lock and runs the
// session under it. Resume takes a different entry point
// (resumeMatrixSessionContext) because it must re-read and mutate the run UNDER
// the lock to be safe against a concurrent resume.
func startMatrixSessionContext(ctx context.Context, run *matrix.Run, combos []matrix.Combination, extraArgs []string, debug bool, obs matrixSessionObserver) (executed []matrix.Result, interrupted bool, err error) {
	if len(combos) == 0 {
		return nil, false, nil
	}

	release, lerr := matrix.AcquireRunLock(run.ID)
	if lerr != nil {
		return nil, false, lerr
	}
	defer release()

	return runMatrixSessionLocked(ctx, run, combos, extraArgs, debug, obs)
}

// runMatrixSessionLocked is the execution core shared by every matrix entry
// point (fresh build, manual retry, resume — local CLI and remote backend
// commands alike). The caller MUST already hold run's execution lock
// (matrix.AcquireRunLock): that is what serializes the incremental history
// writes and guarantees a single executor owns the run. It builds the build
// function from the run's configuration snapshot (never from the current
// phelix.yaml), applies the snapshot's retry budget, records every completed
// combination into the run (and the history store) as it finishes, and
// finalizes the run when done.
//
// obs, when non-nil, observes every attempt start and recorded result after
// they are persisted.
func runMatrixSessionLocked(ctx context.Context, run *matrix.Run, combos []matrix.Combination, extraArgs []string, debug bool, obs matrixSessionObserver) (executed []matrix.Result, interrupted bool, err error) {
	if obs != nil {
		obs.SessionStarted(run)
	}

	buildFn, berr := matrixSessionBuildFunc(run, combos, extraArgs, debug)
	if berr != nil {
		return nil, false, berr
	}

	var warnOnce sync.Once
	journalDir, dirErr := matrix.RunsDir()
	warnJournal := func(err error) {
		warnOnce.Do(func() {
			fmt.Printf("  %s Warning: could not update matrix run history: %v\n", color.YellowString("⚠"), err)
		})
	}

	if dirErr != nil {
		warnJournal(dirErr)
	}

	journalResult := func(c matrix.Combination) {
		if dirErr != nil {
			return
		}
		entry, ok := run.CombinationEntry(c.ID())
		if !ok {
			return
		}
		if err := matrix.AppendJournal(journalDir, run.ID, entry); err != nil {
			warnJournal(err)
		}
	}

	persist := func() {
		if uerr := matrix.UpdateRun(run.Clone()); uerr != nil {
			warnOnce.Do(func() {
				fmt.Printf("  %s Warning: could not update matrix run history: %v\n", color.YellowString("⚠"), uerr)
			})
		}
	}

	onAttempt := func(c matrix.Combination, attempt int) {
		run.MarkRunning(c.ID(), attempt, time.Now())
		persist()
		if obs != nil {
			obs.AttemptStarted(run, c, attempt)
		}
	}

	onResult := func(res matrix.Result) {
		run.RecordResult(res)
		journalResult(res.Combination)
		if obs != nil {
			obs.ResultRecorded(run, res)
		}
	}

	results := matrix.Execute(&matrix.MatrixPlan{Lang: run.Config.Profile().Lang, Combinations: combos}, buildFn, matrix.ExecutorConfig{
		Concurrency: run.Config.Concurrency,
		Debug:       debug,
		Retries:     run.Config.Retries,
		Context:     ctx,
		OnAttempt:   onAttempt,
		OnResult:    onResult,
	})

	run.Finalize(time.Now())
	if uerr := matrix.UpdateRun(run.Clone()); uerr != nil {
		fmt.Printf("  %s Warning: could not record matrix run history: %v\n", color.YellowString("⚠"), uerr)
	}

	for _, res := range results {
		if res.Status != "" {
			executed = append(executed, res)
		}
	}
	return executed, ctx.Err() != nil, nil
}

// resumeMatrixSession is the local CLI resume entry point: signal handling
// around resumeMatrixSessionContext. outside is the run resolved before the
// lock (for the dry-run preview and the app-name guard); the returned run is
// the authoritative one the session executed under the lock.
func resumeMatrixSession(outside *matrix.Run, buildArgsOverride []string, debug bool) (run *matrix.Run, interrupted bool, err error) {
	withMatrixSignals(func(ctx context.Context) {
		run, interrupted, err = resumeMatrixSessionContext(ctx, outside, buildArgsOverride, debug, nil)
	})
	return run, interrupted, err
}

// resumeMatrixSessionContext executes a resume entirely under the run's
// execution lock, closing the resume TOCTOU where two resumes both pass a
// pre-lock Resumable() check and both mutate the run. It acquires the lock
func resumeMatrixSessionContext(ctx context.Context, outside *matrix.Run, buildArgsOverride []string, debug bool, obs matrixSessionObserver) (*matrix.Run, bool, error) {
	release, lerr := matrix.AcquireRunLock(outside.ID)
	if lerr != nil {
		return outside, false, lerr
	}
	defer release()

	run, err := matrix.LoadRun(outside.ID)
	if err != nil {
		return outside, false, err
	}
	if !run.Resumable() {
		return run, false, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix run %s is no longer resumable (status: %s) — another execution may have completed it", run.ID, run.Status)
	}

	run.ResumeCount++
	run.Status = matrix.RunStatusRunning

	if len(buildArgsOverride) > 0 {
		run.Config.BuildArgs = append([]string(nil), buildArgsOverride...)
	}
	if uerr := matrix.UpdateRun(run.Clone()); uerr != nil {
		return run, false, uerr
	}

	combos := run.IncompleteCombinations()
	_, interrupted, err := runMatrixSessionLocked(ctx, run, combos, run.Config.BuildArgs, debug, obs)
	return run, interrupted, err
}

// matrixSessionBuildFunc constructs the combination build function from the
// run's configuration snapshot. Go builds with more than one distinct version
// use per-version Docker containers, exactly like the fresh-build path.
func matrixSessionBuildFunc(run *matrix.Run, combos []matrix.Combination, extraArgs []string, debug bool) (matrix.BuildFunc, error) {
	prof := run.Config.Profile()
	projectRoot := run.ProjectDir
	if projectRoot == "" {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix run %s has no project directory recorded — it predates run snapshots and cannot be re-executed", run.ID)
	}

	versions := make(map[string]bool)
	for _, c := range combos {
		versions[c.Version] = true
	}

	switch prof.Lang {
	case builder.Go:
		gb := &matrix.GoMatrixBuilder{
			ProjectRoot: projectRoot,
			AppName:     run.AppName,
			UseDocker:   len(versions) > 1,
			Debug:       debug,
			ExtraArgs:   extraArgs,
		}
		return gb.Build, nil
	case builder.Rust:
		rb := &matrix.RustMatrixBuilder{ProjectRoot: projectRoot, AppName: run.AppName, Debug: debug, ExtraArgs: extraArgs}
		return rb.Build, nil
	default:
		return nil, phelixerr.Newf(phelixerr.CodeUnsupportedProject,
			"unsupported language for matrix build: %s", prof.Lang)
	}
}

// completeMatrixSession renders the report for the whole run and records every
// successful artifact (including combinations that succeeded in an earlier,
// interrupted session of a resumed run) in the versioning system. The report
// is generated from the run record — not just this session's results — so a
// resumed run's report.json describes all of its combinations, consistent with
// the release manifest and versions.json. Observability only — it never fails
// the session.
func completeMatrixSession(run *matrix.Run, tag string) {
	report := matrix.ReportFromRun(run)
	report.PrintTerminal()

	reportPath, _ := report.WriteJSON(run.ProjectDir)
	if reportPath != "" {
		fmt.Printf("  %s Report written to %s\n", color.GreenString("✓"), reportPath)
	}

	if run.Succeeded == 0 {
		return
	}

	// Record artifacts from the run record (not just this session's results)
	// so a resumed run records its previously-succeeded combinations too.
	// Each artifact carries its checksum — the integrity identity that flows
	// into the release manifest below.
	artifacts := make([]deploy.MatrixArtifact, 0, run.Succeeded)
	successful := make([]matrix.RunCombination, 0, run.Succeeded)
	for _, rc := range run.Combinations {
		if rc.Status != "success" {
			continue
		}
		successful = append(successful, rc)
		artifacts = append(artifacts, deploy.MatrixArtifact{
			MatrixRunID: string(run.ID),
			Platform:    rc.Platform,
			Version:     rc.Version,
			Binary:      rc.Artifact,
			SHA256:      rc.SHA256,
			Status:      rc.Status,
			SizeBytes:   fileSize(rc.Artifact),
			Report:      newMatrixComboReportFromRun(rc),
		})
	}

	gitCommit := deploy.DetectGitCommit(run.ProjectDir)
	logger := &colorLogger{}
	rec, verErr := deploy.RecordMatrixBuild(
		run.AppName, tag, gitCommit, artifacts, "",
		deploy.DefaultRetention{Max: 5}, logger,
	)
	if verErr != nil {
		fmt.Printf("  %s Warning: could not record version: %v\n", color.YellowString("⚠"), verErr)
		return
	}
	fmt.Printf("  %s Matrix build recorded in version history (v%d)\n", color.GreenString("✓"), rec.Version)

	matrixAppID := ""
	for _, a := range app.Manager.ListApplications() {
		if a.Name == run.AppName {
			matrixAppID = a.ID
			break
		}
	}
	phelixgrpc.ReportMatrixBuildEventForApp(matrixAppID, run.AppName,
		run.Status == matrix.RunStatusSucceeded, rec.Version, tag, gitCommit, artifacts, "")

	if run.Status == matrix.RunStatusSucceeded || run.Status == matrix.RunStatusPartial {
		manifest, merr := matrix.BuildReleaseManifest(run, rec.Version, tag, time.Now())
		if merr == nil {
			if serr := matrix.SaveManifest(manifest); serr != nil {
				fmt.Printf("  %s Warning: could not write release manifest: %v\n", color.YellowString("⚠"), serr)
			} else {
				fmt.Printf("  %s Release manifest: v%d (%s, %d artifact(s))\n",
					color.GreenString("✓"), manifest.Version, manifest.Status, len(manifest.Artifacts))
			}
		}
	}

	history, herr := deploy.BuildReportHistory(run.AppName, rec.Version)
	summaries := make([]buildreport.CompactSummary, 0, len(successful))
	for _, rc := range successful {
		s := buildreport.CompactSummary{Combination: rc.ID}
		if herr == nil {
			s.Report = newMatrixComboReportFromRun(rc)
			s.Analysis = buildreport.Analyze(s.Report, history, buildreport.DefaultConfig())
		}
		summaries = append(summaries, s)
	}
	buildreport.PrintCompactSummaries(os.Stdout, summaries)
	if herr != nil {
		fmt.Printf("  %s Build report warning:\n", color.YellowString("⚠"))
		fmt.Printf("  Unable to compare with previous builds: %v\n", herr)
	}
}

// newMatrixComboReportFromRun assembles the per-combination build report from
// a persisted run entry — the recording-time twin of newMatrixComboReport.
func newMatrixComboReportFromRun(rc matrix.RunCombination) *buildreport.Report {
	compiler := rc.Toolchain
	if rc.Toolchain == string(builder.Rust) {
		compiler = "rust/cargo"
	}
	cacheStatus := buildreport.CacheStatus(strings.TrimSpace(rc.CacheStatus))
	if cacheStatus != buildreport.CacheCold && cacheStatus != buildreport.CacheHit {
		cacheStatus = buildreport.CacheUnknown
	}
	return &buildreport.Report{
		Language:        rc.Toolchain,
		Compiler:        compiler,
		CompilerVersion: rc.Version,
		Cache:           buildreport.CacheInfo{Status: cacheStatus},
		Artifact: buildreport.ArtifactInfo{
			Type:      buildreport.ArtifactBinary,
			SizeBytes: fileSize(rc.Artifact),
			Platform:  rc.Platform,
		},
	}
}

// resumeMatrixRun continues an interrupted matrix run: the most recent
// resumable run (optionally filtered by application name), or the run whose
// ID was passed to --resume. Completed combinations are never rebuilt; only
// pending and orphaned-running combinations execute, using the original
// configuration snapshot.
func resumeMatrixRun(args []string, projCfg *project.Config, extraArgs []string, tag string, debug bool) error {
	var run *matrix.Run
	if matrixResume == "latest" {
		appName := ""
		if len(args) > 0 {
			appName = args[0]
		} else if projCfg != nil {
			appName = projCfg.Name
		}
		r, err := matrix.LatestResumableRun(appName)
		if err != nil {
			return err
		}
		run = r
	} else {
		id, err := matrix.ParseRunID(matrixResume)
		if err != nil {
			return err
		}
		r, err := matrix.LoadRun(id)
		if err != nil {
			return err
		}
		if !r.Resumable() {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"matrix run %s has no incomplete combinations (status: %s) — nothing to resume; failed combinations can be retried with 'phelix matrix retry %s --failed'",
				r.ID, r.Status, r.ID)
		}
		run = r
	}

	if len(args) > 0 && args[0] != run.AppName {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix run %s belongs to application %q, not %q", run.ID, run.AppName, args[0])
	}

	combos := run.IncompleteCombinations()

	if matrixDryRun {
		fmt.Printf("%s Dry run: resuming Matrix Run %s (%s) would execute %d of %d combination(s):\n",
			color.BlueString("→"), color.CyanString(string(run.ID)), run.AppName, len(combos), run.Total)
		for _, c := range combos {
			fmt.Printf("    %s %s\n", color.New(color.Faint).Sprint("•"), c.ID())
		}
		fmt.Printf("  %s Dry run — no builds executed; the run record is unchanged\n", color.YellowString("Note:"))
		return nil
	}

	fmt.Printf("%s Resuming Matrix Run %s (%s): %d of %d combinations remaining\n",
		color.BlueString("→"), color.CyanString(string(run.ID)), run.AppName, len(combos), run.Total)

	var buildArgsOverride []string
	if len(extraArgs) > 0 {
		buildArgsOverride = extraArgs
	}

	resumed, interrupted, err := resumeMatrixSession(run, buildArgsOverride, debug)
	if err != nil {
		return err
	}
	completeMatrixSession(resumed, tag)

	if interrupted {
		return phelixerr.Newf(phelixerr.CodeBuildFailed,
			"matrix run %s interrupted — continue it with 'phelix build --matrix --resume'", resumed.ID)
	}
	if resumed.Failed > 0 {
		return phelixerr.Newf(phelixerr.CodeBuildFailed,
			"matrix run %s completed with %d failure(s) out of %d combinations — retry them with 'phelix matrix retry %s --failed'",
			resumed.ID, resumed.Failed, resumed.Total, resumed.ID)
	}
	return nil
}
