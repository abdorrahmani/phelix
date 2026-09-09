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

// startMatrixSession executes combos for run, which must already be persisted
// in its pre-execution state (fresh runs via SaveRun, resumes via UpdateRun).
// It builds the build function from the run's configuration snapshot (never
// from the current phelix.yaml), applies the snapshot's retry budget, records
// every completed combination into the run (and the history store) as it
// finishes, and finalizes the run when done.
//
// interrupted reports whether the session stopped early because the user
// hit SIGINT/SIGTERM: the run is left with status "interrupted" and its
// pending combinations are resumable.
func startMatrixSession(run *matrix.Run, combos []matrix.Combination, extraArgs []string, debug bool) (executed []matrix.Result, interrupted bool, err error) {
	if len(combos) == 0 {
		return nil, false, nil
	}

	release, lerr := matrix.AcquireRunLock(run.ID)
	if lerr != nil {
		return nil, false, lerr
	}
	defer release()

	buildFn, berr := matrixSessionBuildFunc(run, combos, extraArgs, debug)
	if berr != nil {
		return nil, false, berr
	}

	// Signal handling: the first SIGINT/SIGTERM cancels the session — no new
	// combinations start, in-flight builds are killed through their build
	// context, and their results are discarded so those combinations stay
	// pending. A second signal terminates the process the default way.
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
		case <-sessionDone:
		}
	}()

	// Incremental persistence: one history update per completed combination.
	// Failures are best-effort (warned once) — a history write error must
	// never fail a successful build.
	var warnOnce sync.Once
	onResult := func(res matrix.Result) {
		run.RecordResult(res)
		if uerr := matrix.UpdateRun(run); uerr != nil {
			warnOnce.Do(func() {
				fmt.Printf("  %s Warning: could not update matrix run history: %v\n", color.YellowString("⚠"), uerr)
			})
		}
	}

	results := matrix.Execute(&matrix.MatrixPlan{Lang: run.Config.Profile().Lang, Combinations: combos}, buildFn, matrix.ExecutorConfig{
		Concurrency: run.Config.Concurrency,
		Debug:       debug,
		Retries:     run.Config.Retries,
		Context:     ctx,
		OnResult:    onResult,
	})

	run.Finalize(time.Now())
	if uerr := matrix.UpdateRun(run); uerr != nil {
		fmt.Printf("  %s Warning: could not record matrix run history: %v\n", color.YellowString("⚠"), uerr)
	}
	signal.Stop(sigCh)

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

// completeMatrixSession renders the report for the executed combinations and
// records every successful artifact of the whole run (including combinations
// that succeeded in an earlier, interrupted session of a resumed run) in the
// versioning system. Observability only — it never fails the session.
func completeMatrixSession(run *matrix.Run, executed []matrix.Result, tag string) {
	report := matrix.GenerateReport(run.AppName, executed, run.StartedAt)
	report.RunID = string(run.ID)
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
	fmt.Printf("  %s Matrix build recorded in version history\n", color.GreenString("✓"))

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

	executed, interrupted, err := startMatrixSession(run, combos, buildArgs, debug)
	if err != nil {
		return err
	}
	completeMatrixSession(run, executed, tag)

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
