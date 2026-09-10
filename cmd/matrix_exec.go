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

	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/buildreport"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
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

// startMatrixSession executes combos for run, which must already be persisted
// in its pre-execution state (fresh runs via SaveRun, resumes via UpdateRun).
// It is the local CLI entry point: SIGINT/SIGTERM cancels the session (no new
// combinations start, in-flight builds are killed, their combinations stay
// pending), the default signal disposition is restored so a second signal
// terminates the process, and the session core runs to completion.
func startMatrixSession(run *matrix.Run, combos []matrix.Combination, extraArgs []string, debug bool) (executed []matrix.Result, interrupted bool, err error) {
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

	executed, interrupted, err = startMatrixSessionContext(ctx, run, combos, extraArgs, debug, nil)
	signal.Stop(sigCh)
	return executed, interrupted, err
}

// startMatrixSessionContext is the execution core shared by every matrix
// entry point (fresh build, resume, manual retry — local CLI and remote
// backend commands alike). It builds the build function from the run's
// configuration snapshot (never from the current phelix.yaml), applies the
// snapshot's retry budget, records every completed combination into the run
// (and the history store) as it finishes, and finalizes the run when done.
//
// Cancellation comes from ctx (the CLI derives it from signals; the remote
// path derives it from daemon shutdown). interrupted reports whether the
// session stopped early: the run is left with status "interrupted" and its
// pending combinations are resumable.
//
// obs, when non-nil, observes every attempt start and recorded result after
// they are persisted.
func startMatrixSessionContext(ctx context.Context, run *matrix.Run, combos []matrix.Combination, extraArgs []string, debug bool, obs matrixSessionObserver) (executed []matrix.Result, interrupted bool, err error) {
	if len(combos) == 0 {
		return nil, false, nil
	}

	release, lerr := matrix.AcquireRunLock(run.ID)
	if lerr != nil {
		return nil, false, lerr
	}
	defer release()

	if obs != nil {
		obs.SessionStarted(run)
	}

	buildFn, berr := matrixSessionBuildFunc(run, combos, extraArgs, debug)
	if berr != nil {
		return nil, false, berr
	}

	// Incremental persistence: one history update per completed combination
	// (and per attempt start, so the live "running" state is visible to
	// `phelix matrix status` from another process). Failures are best-effort
	// (warned once) — a history write error must never fail a successful
	// build. Every write marshals a clone taken under the run's lock: other
	// workers keep mutating the run while this one serializes.
	var warnOnce sync.Once
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
		persist()
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

	// The release manifest describes this run's artifact set under the
	// logical version just recorded. Only finished runs with successful
	// artifacts form a release: an interrupted run has none yet (its
	// manifest appears once a resume finishes), and a partial run gets an
	// explicitly partial manifest — never a silently complete one.
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

	// An explicit name that disagrees with the run's application is almost
	// certainly the wrong run — fail instead of building something else.
	if len(args) > 0 && args[0] != run.AppName {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix run %s belongs to application %q, not %q", run.ID, run.AppName, args[0])
	}

	combos := run.IncompleteCombinations()

	// --matrix-dry-run previews what a resume would execute without touching
	// the run: no lock, no ResumeCount bump, no history update, no builds.
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

	run.ResumeCount++
	run.Status = matrix.RunStatusRunning
	// Explicit --build-arg flags override the snapshot; otherwise the
	// original build arguments are reproduced.
	buildArgs := run.Config.BuildArgs
	if len(extraArgs) > 0 {
		buildArgs = extraArgs
		run.Config.BuildArgs = append([]string(nil), extraArgs...)
	}
	if uerr := matrix.UpdateRun(run); uerr != nil {
		return uerr
	}

	_, interrupted, err := startMatrixSession(run, combos, buildArgs, debug)
	if err != nil {
		return err
	}
	completeMatrixSession(run, tag)

	if interrupted {
		return phelixerr.Newf(phelixerr.CodeBuildFailed,
			"matrix run %s interrupted — continue it with 'phelix build --matrix --resume'", run.ID)
	}
	if run.Failed > 0 {
		return phelixerr.Newf(phelixerr.CodeBuildFailed,
			"matrix run %s completed with %d failure(s) out of %d combinations — retry them with 'phelix matrix retry %s --failed'",
			run.ID, run.Failed, run.Total, run.ID)
	}
	return nil
}
