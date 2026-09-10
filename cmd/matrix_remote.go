package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/matrix"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/fatih/color"
)

// Remote Build Matrix commands: the backend sends a matrix_* command over
// the MonitorStream, and these handlers run the SAME matrix engine the local
// CLI commands use. There is deliberately no second matrix implementation
// here: profile convergence (matrix.Resolve), plan expansion (Expand),
// execution (startMatrixSessionContext / runDockerizeMatrixCore), report and
// version recording (completeMatrixSession), resume and manual retry are the
// CLI's own code paths — this file only translates command payloads into the
// same calls and projects the results onto the wire.
//
// Termination semantics: the MonitorCommandResult is the terminal outcome of
// the command (success, or the same non-zero-exit semantics the local CLI
// has: BUILD_FAILED for failed/interrupted runs — with the full run state
// attached so the backend sees exactly what succeeded). The detailed
// lifecycle travels separately through MatrixEvents, correlated by
// request_id and matrix_run_id; the backend must not treat the command
// result as the only signal that a run finished.

// RemoteMatrixCommand executes one backend-issued matrix command. ctx is the
// daemon's matrix shutdown context: canceling it interrupts in-flight
// sessions (runs finalize as "interrupted", resumable) instead of killing
// builds mid-write. The result is fully populated; the grpc dispatcher
// stamps identity fields.
func RemoteMatrixCommand(ctx context.Context, req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	opts := req.GetMatrix()
	switch req.GetType() {
	case phelixgrpc.MatrixCommandBuild:
		return remoteMatrixBuild(ctx, req, opts)
	case phelixgrpc.MatrixCommandDockerize:
		return remoteMatrixDockerize(ctx, req, opts)
	case phelixgrpc.MatrixCommandResume:
		return remoteMatrixResume(ctx, req, opts)
	case phelixgrpc.MatrixCommandRetry:
		return remoteMatrixRetry(ctx, req, opts)
	case phelixgrpc.MatrixCommandStatus:
		return remoteMatrixStatus(req, opts)
	case phelixgrpc.MatrixCommandList:
		return remoteMatrixList(req, opts)
	default:
		return remoteMatrixError(req, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"unknown matrix command type %q", req.GetType()))
	}
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func remoteMatrixError(req *pb.MonitorCommandRequest, err error) *pb.MonitorCommandResult {
	return &pb.MonitorCommandResult{
		Status:    "error",
		Error:     err.Error(),
		ErrorCode: string(phelixerr.CodeOf(err)),
	}
}

// remoteMatrixProject resolves the target application and its project
// directory, loads the app's phelix.yaml (missing file = no profile), and
// detects the project language. This is the same app resolution every remote
// command uses (by ID or name); the project directory is where a local
// `phelix build` in that directory would run.
func remoteMatrixProject(name string) (*app.AppInfo, *project.Config, builder.Language, error) {
	appInfo, err := GetAppInfo(name)
	if err != nil {
		return nil, nil, "", err
	}
	projCfg, err := loadProjectConfigFrom(appInfo.Directory)
	if err != nil {
		return nil, nil, "", err
	}
	lang := builder.NewBuildManager().DetectLanguage(appInfo.Directory)
	if !lang.IsSupported() {
		return nil, nil, "", phelixerr.Newf(phelixerr.CodeUnsupportedProject,
			"unsupported or unknown project language in %s: %s", appInfo.Directory, lang)
	}
	return appInfo, projCfg, lang, nil
}

// remoteMatrixResolveInput assembles the convergence input from the command
// payload and the app's phelix.yaml. The payload IS the "CLI" layer of the
// documented precedence (payload > phelix.yaml matrix profile > default,
// list replacement, never merge) — run snapshots record payload-provided
// dimensions with the "cli" source.
func remoteMatrixResolveInput(opts *pb.MatrixOptions, projCfg *project.Config, detectedLang builder.Language) matrix.ResolveInput {
	in := matrix.ResolveInput{
		DetectedLang:  detectedLang,
		MatrixFlag:    true,
		MatrixFlagSet: true,
		CLI: matrix.CLIOptions{
			GoVersions:   opts.GetGoVersions(),
			RustVersions: opts.GetRustVersions(),
			Platforms:    opts.GetPlatforms(),
			Concurrency:  int(opts.GetConcurrency()),
			Retries:      int(opts.GetRetries()),
		},
	}
	if projCfg != nil && projCfg.Matrix != nil {
		in.YAML = projCfg.Matrix.MatrixProfile()
		in.YAMLEnabled = projCfg.Matrix.Enabled
	}
	return in
}

// remoteMatrixProfile resolves and validates the effective profile for a
// build-like command (payload + phelix.yaml + detected language), failing
// with the same convergence errors the local command would report.
func remoteMatrixProfile(opts *pb.MatrixOptions, projCfg *project.Config, lang builder.Language) (*matrix.Profile, error) {
	prof, active, err := matrix.Resolve(remoteMatrixResolveInput(opts, projCfg, lang))
	if err != nil {
		return nil, err
	}
	if !active || prof == nil {
		return nil, phelixerr.New(phelixerr.CodeInvalidArgument,
			"matrix mode is not active — pass go_versions/rust_versions/platforms or enable the phelix.yaml matrix profile")
	}
	if _, err := prof.Plan(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeInvalidArgument, "invalid matrix build plan", err)
	}
	return prof, nil
}

// remoteSessionEvents adapts the session observer to the gRPC reporter.
// resumed distinguishes matrix.resumed (a resume session on an existing run)
// from matrix.started (a fresh run or manual-retry run).
type remoteSessionEvents struct {
	rep     *phelixgrpc.MatrixReporter
	resumed bool
}

func (o remoteSessionEvents) SessionStarted(run *matrix.Run) {
	if o.resumed {
		o.rep.RunResumed(run)
		return
	}
	o.rep.RunStarted(run)
}

func (o remoteSessionEvents) AttemptStarted(_ *matrix.Run, c matrix.Combination, attempt int) {
	o.rep.AttemptStarted(c, attempt)
}

func (o remoteSessionEvents) ResultRecorded(_ *matrix.Run, res matrix.Result) {
	o.rep.ResultRecorded(res)
}

// remoteDockerEvents adapts the docker-matrix observer to the gRPC reporter.
type remoteDockerEvents struct {
	rep  *phelixgrpc.MatrixReporter
	meta map[string]string
}

func (e remoteDockerEvents) Started(plan *matrix.MatrixPlan) { e.rep.DockerStarted(plan, e.meta) }
func (e remoteDockerEvents) AttemptStarted(c matrix.Combination, attempt int) {
	e.rep.AttemptStarted(c, attempt)
}
func (e remoteDockerEvents) ResultRecorded(res matrix.Result) { e.rep.ResultRecorded(res) }

// matrixRunTerminalResult builds the terminal result of a run-based command:
// success for a fully-succeeded run; the CLI's BUILD_FAILED semantics for
// failed or interrupted runs — with the full run state attached either way,
// because a partial matrix is still a result the backend must be able to
// present (which combinations succeeded, with what artifacts).
func matrixRunTerminalResult(run *matrix.Run, interrupted bool, execErr error) *pb.MonitorCommandResult {
	result := &pb.MonitorCommandResult{
		MatrixRunId: string(run.ID),
		MatrixRun:   phelixgrpc.ToProtoMatrixRunState(run),
	}
	switch {
	case execErr != nil:
		result.Status = "error"
		result.Error = execErr.Error()
		result.ErrorCode = string(phelixerr.CodeOf(execErr))
	case interrupted:
		result.Status = "error"
		result.Error = fmt.Sprintf(
			"matrix run %s interrupted — continue it with a matrix_resume command (target %s)",
			run.ID, run.ID)
		result.ErrorCode = string(phelixerr.CodeBuildFailed)
	case run.Failed > 0:
		result.Status = "error"
		result.Error = fmt.Sprintf(
			"matrix run %s completed with %d failure(s) out of %d combinations — retry them with a matrix_retry command (target %s)",
			run.ID, run.Failed, run.Total, run.ID)
		result.ErrorCode = string(phelixerr.CodeBuildFailed)
	default:
		result.Status = "success"
	}
	return result
}

// ---------------------------------------------------------------------------
// matrix_build — native binary matrix (run-based)
// ---------------------------------------------------------------------------

// remoteMatrixBuild executes a native matrix build: the remote twin of
// `phelix build <app> --matrix`. It resolves the effective profile (payload
// dimensions > the app's phelix.yaml matrix profile > default), expands the
// plan, mints a Matrix Run, persists it before the first build, executes the
// session, and records the report/version/manifest exactly like the local
// command (completeMatrixSession).
func remoteMatrixBuild(ctx context.Context, req *pb.MonitorCommandRequest, opts *pb.MatrixOptions) *pb.MonitorCommandResult {
	appInfo, projCfg, lang, err := remoteMatrixProject(req.GetAppName())
	if err != nil {
		return remoteMatrixError(req, err)
	}
	prof, err := remoteMatrixProfile(opts, projCfg, lang)
	if err != nil {
		return remoteMatrixError(req, err)
	}
	plan, perr := prof.Plan()
	if perr != nil {
		return remoteMatrixError(req, phelixerr.Wrap(phelixerr.CodeInvalidArgument, "invalid matrix build plan", perr))
	}

	// Dry run: the plan and nothing else — no run, no lock, no builds, no
	// state mutation (the same preview semantics as --matrix-dry-run).
	if req.GetDryRun() {
		return &pb.MonitorCommandResult{
			Status:        "success",
			MatrixPreview: phelixgrpc.ToProtoMatrixPlanPreview(plan),
		}
	}

	// Fresh run: mint the ID, snapshot the configuration, persist before the
	// first build. A history write failure warns (like the local command) —
	// the build still runs; the run just is not resumable.
	runID := matrix.NewUniqueRunID(time.Now())
	run := matrix.NewRun(runID, appInfo.Name, appInfo.Directory, prof, time.Now())
	run.Config.BuildArgs = append([]string(nil), opts.GetBuildArgs()...)
	run.InitCombinations(plan.Combinations)
	if serr := matrix.SaveRun(run); serr != nil {
		fmt.Printf("  %s Warning: could not record matrix run history: %v\n", color.YellowString("⚠"), serr)
	}

	rep := phelixgrpc.NewMatrixReporter(req.GetRequestId(), string(runID), appInfo.Name, phelixgrpc.MatrixModeNative)
	_, interrupted, serr := startMatrixSessionContext(ctx, run, plan.Combinations, opts.GetBuildArgs(), false, remoteSessionEvents{rep: rep})
	if serr != nil {
		// The session never executed anything (lock or build-function
		// failure). The persisted run stays resumable; the result carries the
		// full state so the backend can see what happened.
		return matrixRunTerminalResult(run, false, serr)
	}

	completeMatrixSession(run, opts.GetTag())
	rep.RunFinished(run, interrupted)
	return matrixRunTerminalResult(run, interrupted, nil)
}

// ---------------------------------------------------------------------------
// matrix_dockerize — docker image matrix (no run)
// ---------------------------------------------------------------------------

// remoteMatrixDockerize executes a docker matrix build: the remote twin of
// `phelix dockerize <app> --matrix`. Image builds have no Matrix Run, no run
// history, no resume, and no release manifest (identical to the local
// command); the terminal result carries the per-combination outcomes with
// image references and digests.
func remoteMatrixDockerize(ctx context.Context, req *pb.MonitorCommandRequest, opts *pb.MatrixOptions) *pb.MonitorCommandResult {
	appInfo, projCfg, lang, err := remoteMatrixProject(req.GetAppName())
	if err != nil {
		return remoteMatrixError(req, err)
	}

	rep := phelixgrpc.NewMatrixReporter(req.GetRequestId(), "", appInfo.Name, phelixgrpc.MatrixModeDocker)
	outcome, err := runDockerizeMatrixCore(dockerizeMatrixParams{
		Name:         appInfo.Name,
		ProjectRoot:  appInfo.Directory,
		Lang:         lang,
		In:           remoteMatrixResolveInput(opts, projCfg, lang),
		RawBuildArgs: opts.GetBuildArgs(),
		Registry:     opts.GetRegistry(),
		Tag:          opts.GetTag(),
		Push:         opts.GetPush(),
		PushPartial:  opts.GetPushPartial(),
		MultiArch:    opts.GetMultiArchTag(),
		DryRun:       req.GetDryRun(),
		Context:      ctx,
	}, remoteDockerEvents{
		rep: rep,
		meta: map[string]string{
			"registry":     opts.GetRegistry(),
			"tag":          opts.GetTag(),
			"push":         fmt.Sprintf("%t", opts.GetPush()),
			"push_partial": fmt.Sprintf("%t", opts.GetPushPartial()),
		},
	})

	// Dry run: the structured preview and nothing else (the core exits before
	// any daemon check, exactly like the local --matrix-dry-run).
	if req.GetDryRun() {
		if err != nil {
			return remoteMatrixError(req, err)
		}
		return &pb.MonitorCommandResult{
			Status:        "success",
			MatrixPreview: phelixgrpc.ToProtoMatrixPlanPreview(outcome.Plan),
		}
	}

	result := &pb.MonitorCommandResult{Status: "success"}
	if outcome != nil && outcome.Report != nil {
		rep.DockerFinished(outcome.Results)
		result.MatrixDocker = dockerOutcomeToProto(opts, outcome)
	}
	if err != nil {
		result.Status = "error"
		result.Error = err.Error()
		result.ErrorCode = string(phelixerr.CodeOf(err))
	}
	return result
}

// dockerOutcomeToProto projects a docker matrix outcome into its wire result.
func dockerOutcomeToProto(opts *pb.MatrixOptions, outcome *dockerizeMatrixOutcome) *pb.MatrixDockerResult {
	out := &pb.MatrixDockerResult{
		AppName:         outcome.Report.AppName,
		Registry:        opts.GetRegistry(),
		Tag:             opts.GetTag(),
		Version:         int32(outcome.Version),
		Pushed:          opts.GetPush(),
		PushPartial:     opts.GetPushPartial(),
		MultiArchImages: append([]string(nil), outcome.MultiArchImages...),
	}
	if outcome.Report != nil {
		pending := outcome.Report.Total - outcome.Report.Succeeded - outcome.Report.Failed - outcome.Report.Skipped
		if pending < 0 {
			pending = 0
		}
		out.Counters = &pb.MatrixCounters{
			Total:     int32(outcome.Report.Total),
			Succeeded: int32(outcome.Report.Succeeded),
			Failed:    int32(outcome.Report.Failed),
			Skipped:   int32(outcome.Report.Skipped),
			Pending:   int32(pending),
		}
	}
	for _, res := range outcome.Results {
		out.Combinations = append(out.Combinations, phelixgrpc.ToProtoMatrixCombinationFromResult(res))
	}
	return out
}

// ---------------------------------------------------------------------------
// matrix_resume — continue an interrupted run
// ---------------------------------------------------------------------------

// remoteMatrixResume continues an interrupted matrix run: the remote twin of
// `phelix build --matrix --resume[=id]`. Completed combinations are never
// rebuilt; only pending and orphaned-running combinations execute, using the
// original configuration snapshot (payload build_args override it, exactly
// like explicit --build-arg flags).
func remoteMatrixResume(ctx context.Context, req *pb.MonitorCommandRequest, opts *pb.MatrixOptions) *pb.MonitorCommandResult {
	run, err := remoteResolveResumeTarget(req)
	if err != nil {
		return remoteMatrixError(req, err)
	}
	combos := run.IncompleteCombinations()

	// Dry run: preview what a resume would execute without touching the run
	// — no lock, no ResumeCount bump, no history update, no builds.
	if req.GetDryRun() {
		preview := &pb.MatrixPlanPreview{
			ResumeRunId: string(run.ID),
			RunTotal:    int32(run.Total),
		}
		for _, c := range combos {
			preview.ExecuteIds = append(preview.ExecuteIds, c.ID())
		}
		return &pb.MonitorCommandResult{Status: "success", MatrixPreview: preview}
	}

	run.ResumeCount++
	run.Status = matrix.RunStatusRunning
	buildArgs := run.Config.BuildArgs
	if len(opts.GetBuildArgs()) > 0 {
		buildArgs = opts.GetBuildArgs()
		run.Config.BuildArgs = append([]string(nil), opts.GetBuildArgs()...)
	}
	if uerr := matrix.UpdateRun(run); uerr != nil {
		return remoteMatrixError(req, uerr)
	}

	rep := phelixgrpc.NewMatrixReporter(req.GetRequestId(), string(run.ID), run.AppName, phelixgrpc.MatrixModeNative)
	_, interrupted, serr := startMatrixSessionContext(ctx, run, combos, buildArgs, false, remoteSessionEvents{rep: rep, resumed: true})
	if serr != nil {
		return matrixRunTerminalResult(run, false, serr)
	}

	completeMatrixSession(run, opts.GetTag())
	rep.RunFinished(run, interrupted)
	return matrixRunTerminalResult(run, interrupted, nil)
}

// remoteResolveResumeTarget resolves the run a matrix_resume command
// continues: "latest" (newest resumable, optionally filtered by app_name) or
// an explicit run ID. An explicit app_name that disagrees with the run's
// application fails — the same guard the local command applies.
func remoteResolveResumeTarget(req *pb.MonitorCommandRequest) (*matrix.Run, error) {
	var run *matrix.Run
	if req.GetTarget() == "latest" {
		r, err := matrix.LatestResumableRun(req.GetAppName())
		if err != nil {
			return nil, err
		}
		run = r
	} else {
		id, err := matrix.ParseRunID(req.GetTarget())
		if err != nil {
			return nil, err
		}
		r, err := matrix.LoadRun(id)
		if err != nil {
			return nil, err
		}
		if !r.Resumable() {
			return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"matrix run %s has no incomplete combinations (status: %s) — nothing to resume; failed combinations can be retried with a matrix_retry command (target %s)",
				r.ID, r.Status, r.ID)
		}
		run = r
	}
	if req.GetAppName() != "" && req.GetAppName() != run.AppName {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix run %s belongs to application %q, not %q", run.ID, run.AppName, req.GetAppName())
	}
	return run, nil
}

// ---------------------------------------------------------------------------
// matrix_retry — failed combinations in a new, linked run
// ---------------------------------------------------------------------------

// remoteMatrixRetry retries a run's failed combinations in a NEW run that
// records its parent: the remote twin of `phelix matrix retry <id> --failed`.
// The source run is never modified. There is no dry-run (the local command
// has none either).
func remoteMatrixRetry(ctx context.Context, req *pb.MonitorCommandRequest, _ *pb.MatrixOptions) *pb.MonitorCommandResult {
	id, err := matrix.ParseRunID(req.GetTarget())
	if err != nil {
		return remoteMatrixError(req, err)
	}
	source, err := matrix.LoadRun(id)
	if err != nil {
		return remoteMatrixError(req, err)
	}
	if req.GetAppName() != "" && req.GetAppName() != source.AppName {
		return remoteMatrixError(req, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix run %s belongs to application %q, not %q", source.ID, source.AppName, req.GetAppName()))
	}
	combos := source.FailedCombinations()
	if len(combos) == 0 {
		return remoteMatrixError(req, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix run %s has no failed combinations — nothing to retry (status: %s)", source.ID, source.Status))
	}
	if source.ProjectDir == "" {
		return remoteMatrixError(req, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix run %s has no project directory recorded — it predates run snapshots and cannot be retried", source.ID))
	}

	retryID := matrix.NewUniqueRunID(time.Now())
	retryRun := matrix.NewRetryRun(retryID, source, time.Now())
	retryRun.InitCombinations(combos)
	if serr := matrix.SaveRun(retryRun); serr != nil {
		return remoteMatrixError(req, phelixerr.Wrap(phelixerr.CodeFilesystem, "could not record retry run", serr))
	}

	rep := phelixgrpc.NewMatrixReporter(req.GetRequestId(), string(retryID), retryRun.AppName, phelixgrpc.MatrixModeNative)
	_, interrupted, serr := startMatrixSessionContext(ctx, retryRun, combos, retryRun.Config.BuildArgs, false, remoteSessionEvents{rep: rep})
	if serr != nil {
		return matrixRunTerminalResult(retryRun, false, serr)
	}

	completeMatrixSession(retryRun, "")
	rep.RunFinished(retryRun, interrupted)
	return matrixRunTerminalResult(retryRun, interrupted, nil)
}

// ---------------------------------------------------------------------------
// matrix_status / matrix_list — read-only queries
// ---------------------------------------------------------------------------

// remoteMatrixStatus answers "what is the state of a run right now": the
// remote twin of `phelix matrix status`. Target semantics:
//
//	target "" or "active" — the currently executing run (app_name optionally
//	  filters when several execute). With no active run, the answer is a
//	  success result with matrix_run unset and matrix_runs holding the most
//	  recent summary (or empty when there is no history) — nothing executing,
//	  never a fabricated run state.
//	target "latest"       — the most recent run of any status.
//	target <run ID>       — that specific run (app_name, when set, must match).
func remoteMatrixStatus(req *pb.MonitorCommandRequest, _ *pb.MatrixOptions) *pb.MonitorCommandResult {
	target := req.GetTarget()

	if target == "" || target == "active" {
		active, err := matrix.ActiveRuns()
		if err != nil {
			return remoteMatrixError(req, err)
		}
		for _, run := range active { // newest first
			if req.GetAppName() != "" && run.AppName != req.GetAppName() {
				continue
			}
			return &pb.MonitorCommandResult{
				Status:      "success",
				MatrixRunId: string(run.ID),
				MatrixRun:   phelixgrpc.ToProtoMatrixRunState(run),
			}
		}
		// No active run: point at the most recent one for context, mirroring
		// the local command's "No active Matrix Run. Most recent run: …".
		runs, _, lerr := matrix.ListRuns()
		if lerr != nil {
			return remoteMatrixError(req, lerr)
		}
		result := &pb.MonitorCommandResult{Status: "success"}
		for _, run := range runs {
			if req.GetAppName() != "" && run.AppName != req.GetAppName() {
				continue
			}
			result.MatrixRuns = []*pb.MatrixRunSummary{phelixgrpc.ToProtoMatrixRunSummary(run)}
			break
		}
		return result
	}

	var run *matrix.Run
	if target == "latest" {
		runs, _, err := matrix.ListRuns()
		if err != nil {
			return remoteMatrixError(req, err)
		}
		for _, r := range runs {
			if req.GetAppName() != "" && r.AppName != req.GetAppName() {
				continue
			}
			run = r
			break
		}
		if run == nil {
			return remoteMatrixError(req, phelixerr.Newf(phelixerr.CodeNotFound,
				"no matrix runs recorded%s — trigger one with a matrix_build command", statusFilterSuffix(req.GetAppName())))
		}
	} else {
		id, err := matrix.ParseRunID(target)
		if err != nil {
			return remoteMatrixError(req, err)
		}
		run, err = matrix.LoadRun(id)
		if err != nil {
			return remoteMatrixError(req, err)
		}
		if req.GetAppName() != "" && req.GetAppName() != run.AppName {
			return remoteMatrixError(req, phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"matrix run %s belongs to application %q, not %q", run.ID, run.AppName, req.GetAppName()))
		}
	}
	return &pb.MonitorCommandResult{
		Status:      "success",
		MatrixRunId: string(run.ID),
		MatrixRun:   phelixgrpc.ToProtoMatrixRunState(run),
	}
}

func statusFilterSuffix(appName string) string {
	if appName == "" {
		return ""
	}
	return " for application " + appName
}

// remoteMatrixList answers with run summaries, newest first: the remote twin
// of `phelix matrix list`. app_name optionally filters by application;
// list_limit bounds the answer (default 20, hard cap 100).
func remoteMatrixList(req *pb.MonitorCommandRequest, opts *pb.MatrixOptions) *pb.MonitorCommandResult {
	runs, _, err := matrix.ListRuns()
	if err != nil {
		return remoteMatrixError(req, err)
	}
	limit := int(opts.GetListLimit())
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	result := &pb.MonitorCommandResult{Status: "success"}
	count := 0
	for _, run := range runs {
		if count >= limit {
			break
		}
		if req.GetAppName() != "" && run.AppName != req.GetAppName() {
			continue
		}
		result.MatrixRuns = append(result.MatrixRuns, phelixgrpc.ToProtoMatrixRunSummary(run))
		count++
	}
	return result
}
