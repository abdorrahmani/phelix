package grpc

import (
	"os"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/matrix"
)

// Conversions between the matrix engine's entities (matrix.Run,
// matrix.RunCombination, matrix.Result, …) and their wire representations
// (pb.MatrixRunState, …). There is exactly one conversion per entity and one
// direction — the wire protocol is a projection of the engine's own state,
// never a second state model.

// unixMilli converts a time to the wire timestamp format; the zero time
// becomes 0 (the same convention deployment_convert.go uses).
func matrixUnixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// durationMS converts a persisted Go duration string ("1.234s") into
// milliseconds. Unparseable or empty values (legacy records, never-started
// combinations) yield 0 — durations are telemetry, never control flow.
func durationMS(s string) int64 {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d.Milliseconds()
}

// fileSizeOfStats returns the artifact's size in bytes; image references and
// missing files yield 0 (mirroring the release manifest's behavior — for
// images the digest is the identity).
func fileSizeOfStats(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// toProtoCounters projects the run's consistent per-status snapshot.
func toProtoCounters(c matrix.StatusCounters) *pb.MatrixCounters {
	return &pb.MatrixCounters{
		Total:     int32(c.Total),
		Succeeded: int32(c.Succeeded),
		Failed:    int32(c.Failed),
		Skipped:   int32(c.Skipped),
		Running:   int32(c.Running),
		Pending:   int32(c.Pending),
	}
}

// countersFromRun takes one consistent snapshot under the run's lock.
func countersFromRun(run *matrix.Run) *pb.MatrixCounters {
	if run == nil {
		return &pb.MatrixCounters{}
	}
	return toProtoCounters(run.SnapshotCounters())
}

// toProtoRule projects one include/exclude rule of a run's configuration
// snapshot.
func toProtoRule(r matrix.Rule) *pb.MatrixRule {
	out := &pb.MatrixRule{}
	if len(r.Dimensions) > 0 {
		out.Dimensions = make(map[string]string, len(r.Dimensions))
		for k, v := range r.Dimensions {
			out.Dimensions[k] = v
		}
	}
	if len(r.Metadata) > 0 {
		out.Metadata = make(map[string]string, len(r.Metadata))
		for k, v := range r.Metadata {
			out.Metadata[k] = v
		}
	}
	return out
}

// toProtoRunConfig projects the run's configuration snapshot.
func toProtoRunConfig(cfg matrix.RunConfig) *pb.MatrixRunConfig {
	out := &pb.MatrixRunConfig{
		Language:    cfg.Lang,
		Versions:    append([]string(nil), cfg.Versions...),
		Platforms:   append([]string(nil), cfg.Platforms...),
		Concurrency: int32(cfg.Concurrency),
		Retries:     int32(cfg.Retries),
		BuildArgs:   append([]string(nil), cfg.BuildArgs...),
		Sources: &pb.MatrixRunConfigSources{
			Language:    cfg.Sources.Lang,
			Versions:    cfg.Sources.Versions,
			Platforms:   cfg.Sources.Platforms,
			Concurrency: cfg.Sources.Concurrency,
			Retries:     cfg.Sources.Retries,
			Include:     cfg.Sources.Include,
			Exclude:     cfg.Sources.Exclude,
		},
	}
	for _, r := range cfg.Include {
		out.Include = append(out.Include, toProtoRule(r))
	}
	for _, r := range cfg.Exclude {
		out.Exclude = append(out.Exclude, toProtoRule(r))
	}
	return out
}

// ToProtoMatrixCombinationFromResult projects a live executor result into its
// wire combination state (used for per-combination events of docker matrix
// builds, which have no run record). Errors are redacted exactly as the run
// history redacts them.
func ToProtoMatrixCombinationFromResult(res matrix.Result) *pb.MatrixCombinationState {
	out := &pb.MatrixCombinationState{
		Id:               res.Combination.ID(),
		Toolchain:        string(res.Combination.Lang),
		ToolchainVersion: res.Combination.Version,
		Os:               res.Combination.OS,
		Arch:             res.Combination.Arch,
		Variant:          res.Combination.Variant,
		Platform:         res.Combination.Platform,
		Status:           res.Status,
		DurationMs:       res.Duration.Milliseconds(),
		Artifact:         res.Artifact,
		Sha256:           res.SHA256,
		SizeBytes:        fileSizeOfStats(res.Artifact),
		CacheStatus:      res.CacheStatus,
	}
	if res.Error != nil {
		out.Error = phelixerr.Redact(res.Error.Error())
	}
	attempts := len(res.Attempts)
	if attempts == 0 && res.Status != "" {
		attempts = 1
	}
	out.Attempts = int32(attempts)
	for _, a := range res.Attempts {
		entry := &pb.MatrixAttemptState{
			Number:     int32(a.Number),
			Status:     a.Status,
			DurationMs: a.Duration.Milliseconds(),
		}
		if a.Error != nil {
			entry.Error = phelixerr.Redact(a.Error.Error())
		}
		out.AttemptLog = append(out.AttemptLog, entry)
	}
	if len(res.Combination.Metadata) > 0 {
		out.Metadata = make(map[string]string, len(res.Combination.Metadata))
		for k, v := range res.Combination.Metadata {
			out.Metadata[k] = v
		}
	}
	return out
}

// toProtoCombination projects one persisted run-combination entry. Artifact
// errors are already redacted at record time; sizes are statted best-effort.
func toProtoCombination(rc matrix.RunCombination) *pb.MatrixCombinationState {
	out := &pb.MatrixCombinationState{
		Id:               rc.ID,
		Identity:         rc.Identity,
		Toolchain:        rc.Toolchain,
		ToolchainVersion: rc.Version,
		Os:               rc.OS,
		Arch:             rc.Arch,
		Variant:          rc.Variant,
		Platform:         rc.Platform,
		Status:           rc.Status,
		DurationMs:       durationMS(rc.Duration),
		Artifact:         rc.Artifact,
		Sha256:           rc.SHA256,
		SizeBytes:        fileSizeOfStats(rc.Artifact),
		StartedAt:        matrixUnixMilli(rc.StartedAt),
		Error:            rc.Error,
		CacheStatus:      rc.CacheStatus,
		Attempts:         int32(rc.Attempts),
	}
	if rc.Status == "success" || rc.Status == "failed" {
		if rc.Attempts == 0 {
			out.Attempts = 1
		}
	}
	for _, a := range rc.AttemptLog {
		out.AttemptLog = append(out.AttemptLog, &pb.MatrixAttemptState{
			Number:     int32(a.Number),
			Status:     a.Status,
			DurationMs: durationMS(a.Duration),
			Error:      a.Error,
		})
	}
	if len(rc.Metadata) > 0 {
		out.Metadata = make(map[string]string, len(rc.Metadata))
		for k, v := range rc.Metadata {
			out.Metadata[k] = v
		}
	}
	return out
}

// ToProtoMatrixRunState projects a complete Matrix Run into its wire state:
// identity, configuration snapshot, timing, a consistent counter snapshot,
// every combination entry, live-execution flags (run lock), and the release
// view when a manifest exists. It reads the run's persisted record only —
// there is no second state to drift.
func ToProtoMatrixRunState(run *matrix.Run) *pb.MatrixRunState {
	if run == nil {
		return nil
	}
	state := &pb.MatrixRunState{
		MatrixRunId: string(run.ID),
		AppName:     run.AppName,
		ProjectDir:  run.ProjectDir,
		Status:      string(run.Status),
		Config:      toProtoRunConfig(run.Config),
		StartedAt:   matrixUnixMilli(run.StartedAt),
		FinishedAt:  matrixUnixMilli(run.FinishedAt),
		DurationMs:  durationMS(run.Duration),
		Counters:    countersFromRun(run),
		ParentRunId: string(run.ParentRunID),
		ResumeCount: int32(run.ResumeCount),
	}
	for _, rc := range run.Combinations {
		state.Combinations = append(state.Combinations, toProtoCombination(rc))
	}
	if executing, pid := matrix.RunLockOwner(run.ID); executing {
		state.Active = true
		state.LockPid = int32(pid)
	}
	if manifest, err := matrix.LoadManifest(run.ID); err == nil && manifest != nil {
		state.Release = &pb.MatrixReleaseInfo{
			Version:           int32(manifest.Version),
			Tag:               manifest.Tag,
			Status:            manifest.Status,
			Artifacts:         int32(len(manifest.Artifacts)),
			TotalCombinations: int32(manifest.TotalCombinations),
		}
	}
	return state
}

// ToProtoMatrixRunSummary projects the list-view of one run (matrix_list).
func ToProtoMatrixRunSummary(run *matrix.Run) *pb.MatrixRunSummary {
	if run == nil {
		return nil
	}
	return &pb.MatrixRunSummary{
		MatrixRunId: string(run.ID),
		AppName:     run.AppName,
		Status:      string(run.Status),
		Counters:    countersFromRun(run),
		ParentRunId: string(run.ParentRunID),
		ResumeCount: int32(run.ResumeCount),
		StartedAt:   matrixUnixMilli(run.StartedAt),
		FinishedAt:  matrixUnixMilli(run.FinishedAt),
		DurationMs:  durationMS(run.Duration),
	}
}

// ToProtoMatrixPlanPreview projects an expanded plan (build/dockerize
// dry-run). Resume previews additionally name the run and the subset a resume
// would execute.
func ToProtoMatrixPlanPreview(plan *matrix.MatrixPlan) *pb.MatrixPlanPreview {
	if plan == nil {
		return nil
	}
	out := &pb.MatrixPlanPreview{
		Language:      string(plan.Lang),
		BaseCount:     int32(plan.BaseCount),
		IncludedCount: int32(plan.IncludedCount),
		ExcludedCount: int32(plan.ExcludedCount),
	}
	for _, c := range plan.Combinations {
		out.Combinations = append(out.Combinations, c.ID())
	}
	return out
}
