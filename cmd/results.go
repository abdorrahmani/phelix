package cmd

// Machine-contract result shapes. These structs are the stable `result`
// payloads of the --json envelopes; add fields only (additive), never rename
// or repurpose existing JSON keys. Referenced shapes are documented in
// docs/reference/machine-contract.md.

// rebuildResult is the terminal result of phelix rebuild (classic,
// blue-green, rolling and canary/progressive paths).
type rebuildResult struct {
	App      string `json:"app"`
	AppID    string `json:"app_id"`
	Version  int    `json:"version,omitempty"`
	Port     int    `json:"port,omitempty"`
	Strategy string `json:"strategy"`
}

// rollbackResult is the terminal result of phelix rollback.
type rollbackResult struct {
	App          string `json:"app"`
	FromVersion  int    `json:"from_version,omitempty"`
	ToVersion    int    `json:"to_version,omitempty"`
	Strategy     string `json:"strategy,omitempty"`
	Port         int    `json:"port,omitempty"`
	Verification string `json:"verification,omitempty"`
}

// buildResult is the terminal result of a plain (non-matrix) phelix build.
type buildResult struct {
	App     string `json:"app"`
	AppID   string `json:"app_id,omitempty"`
	Version int    `json:"version,omitempty"`
	Path    string `json:"path,omitempty"`
}

// matrixBuildResult is the terminal result of a matrix build. The operation
// identity of a matrix build is the mx_ run ID (its own durable record); the
// full per-combination detail lives in report_path.
type matrixBuildResult struct {
	RunID      string `json:"run_id"`
	App        string `json:"app"`
	Total      int    `json:"total"`
	Succeeded  int    `json:"succeeded"`
	Failed     int    `json:"failed"`
	Skipped    int    `json:"skipped"`
	ReportPath string `json:"report_path,omitempty"`
}

// rollbackPlanResult is the read-only result of `rollback --dry-run --json`.
type rollbackPlanResult struct {
	App                  string   `json:"app"`
	CurrentVersion       int      `json:"current_version"`
	TargetVersion        int      `json:"target_version"`
	CurrentTag           string   `json:"current_tag,omitempty"`
	TargetTag            string   `json:"target_tag,omitempty"`
	TargetCommit         string   `json:"target_commit,omitempty"`
	Strategy             string   `json:"strategy"`
	Replicas             int      `json:"replicas,omitempty"`
	PublicPort           int      `json:"public_port,omitempty"`
	CurrentSlot          string   `json:"current_slot,omitempty"`
	TargetSlot           string   `json:"target_slot,omitempty"`
	HealthCheck          string   `json:"health_check,omitempty"`
	Downtime             bool     `json:"downtime"`
	EnvSnapshotAvailable bool     `json:"env_snapshot_available,omitempty"`
	Steps                []string `json:"steps,omitempty"`
	Warnings             []string `json:"warnings,omitempty"`
	TargetBuiltAt        int64    `json:"target_built_at_ms,omitempty"`
}

// rollbackVersionView is one retained version in a `rollback --list --json`
// response.
type rollbackVersionView struct {
	Version   int    `json:"version"`
	Tag       string `json:"tag,omitempty"`
	GitCommit string `json:"git_commit,omitempty"`
	BuiltAt   int64  `json:"built_at_ms"`
	SizeBytes int64  `json:"size_bytes"`
	Current   bool   `json:"current"`
	PruneSoon bool   `json:"prune_soon"`
}

// rollbackListResult is the result of `rollback --list --json`.
type rollbackListResult struct {
	App      string                `json:"app"`
	Count    int                   `json:"count"`
	Versions []rollbackVersionView `json:"versions"`
}

// deployUnlockResult is the result of `deploy unlock --json`.
type deployUnlockResult struct {
	App      string `json:"app"`
	Unlocked bool   `json:"unlocked"`
}
