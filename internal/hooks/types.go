package hooks

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusTimedOut  = "timed_out"
	StatusCancelled = "cancelled"
	StatusSkipped   = "skipped"
	StatusUncertain = "uncertain"
)

// IsTerminal reports whether a status is final.
func IsTerminal(status string) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusTimedOut, StatusCancelled, StatusSkipped:
		return true
	}
	return false
}

type Execution struct {
	ID              string    `json:"id"`
	AppName         string    `json:"app_name"`
	AppID           string    `json:"app_id,omitempty"`
	DeploymentID    string    `json:"deployment_id"`
	Strategy        string    `json:"strategy,omitempty"`
	Hook            string    `json:"hook"`
	Attempt         int       `json:"attempt"`
	Version         int       `json:"version,omitempty"`
	PreviousVersion int       `json:"previous_version,omitempty"`
	Command         []string  `json:"command"`
	WorkingDir      string    `json:"working_dir"`
	Status          string    `json:"status"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at,omitempty"`
	DurationMS      int64     `json:"duration_ms"`
	ExitCode        *int      `json:"exit_code,omitempty"`
	Stdout          string    `json:"stdout,omitempty"`
	Stderr          string    `json:"stderr,omitempty"`
	StdoutTruncated bool      `json:"stdout_truncated,omitempty"`
	StderrTruncated bool      `json:"stderr_truncated,omitempty"`
	FailureReason   string    `json:"failure_reason,omitempty"`
	TimeoutMS       int64     `json:"timeout_ms"`
	ContinueOnError bool      `json:"continue_on_error"`
	OwnerPID        int       `json:"owner_pid,omitempty"`
}

type Env struct {
	AppName         string
	AppID           string
	ProjectDir      string
	DeploymentID    string
	Strategy        string
	Version         int
	PreviousVersion int
	Hook            string
	Attempt         int
}

// Variables renders the context variables. They are added after the process
// environment and before the hook's own env block, which may override neither
// the reserved PHELIX_ prefix (config validation) nor these values.
func (e Env) Variables() []string {
	return []string{
		"PHELIX_APP=" + e.AppName,
		"PHELIX_APP_ID=" + e.AppID,
		"PHELIX_PROJECT_DIR=" + e.ProjectDir,
		"PHELIX_DEPLOYMENT_ID=" + e.DeploymentID,
		"PHELIX_STRATEGY=" + e.Strategy,
		"PHELIX_VERSION=" + fmt.Sprintf("%d", e.Version),
		"PHELIX_PREVIOUS_VERSION=" + fmt.Sprintf("%d", e.PreviousVersion),
		"PHELIX_HOOK=" + e.Hook,
		"PHELIX_ATTEMPT=" + fmt.Sprintf("%d", e.Attempt),
	}
}

// newExecutionID builds a stable, unique identity for one attempt.
func newExecutionID(deploymentID, hook string, attempt int) string {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		suffix = nil
	}
	base := fmt.Sprintf("%s-%s-%d", deploymentID, hook, attempt)
	if suffix == nil {
		return fmt.Sprintf("%s-%d", base, time.Now().UnixNano())
	}
	return base + "-" + hex.EncodeToString(suffix)
}

// resolveWorkingDir confines dir to root. Empty means root itself.
func resolveWorkingDir(root, dir string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target := root
	if strings.TrimSpace(dir) != "" {
		target, err = filepath.Abs(filepath.Join(root, dir))
		if err != nil {
			return "", err
		}
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("working directory %q escapes the project directory", dir)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		return "", fmt.Errorf("working directory %q does not exist", dir)
	}
	return target, nil
}
