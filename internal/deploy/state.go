// Package deploy implements Phelix's zero-downtime deployment strategies:
// blue-green (internal/deploy/bluegreen.go) and rolling
// (internal/deploy/rolling.go). It owns its own per-app state file so the
// existing single-PID AppInfo model used by start/stop/status is left intact.
//
// State layout:
//
//	~/.phelix/apps/<AppName>/deploy.json
//
// Each DeployState records the public port, the active slot/replica, the
// blue/green slots (or the replica list), and a summary of the health tier
// last used so phelix status and other commands can read it.
package deploy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Slot names for blue-green deploys.
const (
	SlotBlue  = "blue"
	SlotGreen = "green"
)

// Mode is the deployment strategy recorded for an app.
type Mode string

const (
	ModeBlueGreen Mode = "blue-green"
	ModeRolling   Mode = "rolling"
)

// Instance describes one running backend instance behind the proxy.
type Instance struct {
	// Slot is the blue-green slot name ("blue"/"green") or, for rolling, a
	// replica index string ("0", "1", ...).
	Slot string `json:"slot"`
	// PID is the OS process id of the instance, or 0 when stopped.
	PID int `json:"pid"`
	// Port is the internal port the instance listens on. The proxy dials this.
	Port int `json:"port"`
	// BinaryPath is the absolute path to the executable the instance runs.
	BinaryPath string `json:"binary_path,omitempty"`
	// StartedAt records when the instance was launched.
	StartedAt time.Time `json:"started_at,omitempty"`
	// Status is one of: "running", "stopped", "failed", "starting".
	Status string `json:"status"`
	// Version is the builds/vN label this instance was started from (when known).
	Version int `json:"version,omitempty"`
	// EnvPath is the encrypted env snapshot (env/vN.enc) paired with the
	// binary this instance runs. Recorded so StartDeployment can relaunch the
	// instance with the same environment later.
	EnvPath string `json:"env_path,omitempty"`
}

// RollbackRecord is a compact summary of the last successful rollback for an
// app, stored in DeployState so phelix status can display it.
type RollbackRecord struct {
	FromVersion int       `json:"from_version"`
	ToVersion   int       `json:"to_version"`
	At          time.Time `json:"at"`
}

// HealthSummary records which health tier was last used for the app and its
// last-known outcome, so status output can surface it without re-probing.
type HealthSummary struct {
	// Tier is the numeric health.Tier in use (1,2,3,4). Stored as int to avoid
	// importing the health package here (keeps the state struct serializable
	// and dependency-free).
	Tier int `json:"tier"`
	// TierLabel is a human description, e.g. "Tier 2 (HTTP probe ...)".
	TierLabel string `json:"tier_label,omitempty"`
	// HealthyAt is when the instance last passed its health window.
	HealthyAt time.Time `json:"healthy_at,omitempty"`
}

// DeployState is the persisted, per-app deploy state. It is the deploy
// package's analogue of the app package's AppInfo, scoped to blue-green /
// rolling deploys.
type DeployState struct {
	// AppName is the app this state belongs to (and the directory name).
	AppName string `json:"app_name"`
	// AppID is the numeric ID from AppInfo, kept for cross-referencing.
	AppID string `json:"app_id,omitempty"`
	// Mode is the active deployment strategy.
	Mode Mode `json:"mode"`
	// PublicPort is the externally exposed port the user asked for. It is
	// stable for the life of the app and is the only port the client ever sees.
	PublicPort int `json:"public_port"`
	// ActiveSlot is the blue-green slot currently receiving traffic. Empty
	// before the first successful deploy.
	ActiveSlot string `json:"active_slot,omitempty"`
	// Slots holds the blue/green instances keyed by SlotBlue/SlotGreen.
	// Only meaningful when Mode == ModeBlueGreen.
	Slots map[string]*Instance `json:"slots,omitempty"`
	// Replicas holds the replica instances for rolling deploys, keyed by index.
	// Only meaningful when Mode == ModeRolling.
	Replicas map[string]*Instance `json:"replicas,omitempty"`
	// Health is the last health tier used and when it passed.
	Health *HealthSummary `json:"health,omitempty"`
	// GraceSeconds is the graceful-shutdown grace period for old instances.
	GraceSeconds int `json:"grace_seconds,omitempty"`
	// ActiveVersion is the builds/vN label currently serving traffic after the
	// last successful deploy or rollback.
	ActiveVersion int `json:"active_version,omitempty"`
	// LastRollback records when the last successful rollback completed, along
	// with from/to version. Useful for phelix status output and audit.
	LastRollback *RollbackRecord `json:"last_rollback,omitempty"`
	// OpLock is set while a blue-green/rolling deploy or rollback is in flight.
	OpLock *DeployLock `json:"op_lock,omitempty"`
	// LastDeploymentID is the telemetry id of the most recent deployment
	// operation on this app (see internal/deploy/telemetry.go). It is recorded
	// so a snapshot rebuilt later — by the monitor daemon after a reconnect, or
	// by another process — can be correlated with the events that deployment
	// emitted. Absent for deployments made before telemetry existed.
	LastDeploymentID string `json:"last_deployment_id,omitempty"`
	// LastRequestID correlates the most recent deployment topology with the
	// backend command that initiated it. Empty for local deployments.
	LastRequestID string `json:"last_request_id,omitempty"`
	// UpdatedAt is when the state was last written.
	UpdatedAt time.Time `json:"updated_at"`
}

// InactiveSlot returns the blue-green slot name opposite to the current active
// slot. If no slot is active yet, blue is deployed first.
func (s *DeployState) InactiveSlot() string {
	if s.ActiveSlot == SlotBlue {
		return SlotGreen
	}
	return SlotBlue
}

// modeSnapshot captures the fields that define which strategy a DeployState
// represents, so a failed strategy migration can be undone before the new
// strategy has replaced anything.
type modeSnapshot struct {
	mode       Mode
	activeSlot string
	slots      map[string]*Instance
	replicas   map[string]*Instance
}

func captureMode(s *DeployState) modeSnapshot {
	return modeSnapshot{mode: s.Mode, activeSlot: s.ActiveSlot, slots: s.Slots, replicas: s.Replicas}
}

func (s *DeployState) restoreMode(m modeSnapshot) {
	s.Mode, s.ActiveSlot, s.Slots, s.Replicas = m.mode, m.activeSlot, m.slots, m.replicas
}

// MigrateTo switches the recorded strategy and retires every instance that
// belonged to the previous one. The retired instances are returned so the
// caller can stop their processes at the right moment — after the new
// strategy's instances actually serve traffic; stopping them earlier would
// drop requests. A no-op returning (nil, nil) when already in mode.
//
// The mode switch is persisted immediately: a crash mid-rollout must not
// leave the old strategy recorded as the active one. Callers that must keep
// the previous strategy when the rollout fails before it replaced anything
// capture the four fields with captureMode and restore them with restoreMode.
func (s *DeployState) MigrateTo(mode Mode) ([]*Instance, error) {
	if s == nil || s.Mode == mode {
		return nil, nil
	}
	var retired []*Instance
	for _, inst := range s.Slots {
		if inst != nil && inst.PID > 0 {
			retired = append(retired, inst)
		}
	}
	for _, inst := range s.Replicas {
		if inst != nil && inst.PID > 0 {
			retired = append(retired, inst)
		}
	}
	s.Slots = nil
	s.Replicas = nil
	s.ActiveSlot = ""
	s.Mode = mode
	if mode == ModeBlueGreen {
		s.Slots = map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, Status: "stopped"},
			SlotGreen: {Slot: SlotGreen, Status: "stopped"},
		}
	} else {
		s.Replicas = make(map[string]*Instance)
	}
	if err := Store(s); err != nil {
		return nil, err
	}
	return retired, nil
}

// ActiveInstance returns the currently serving instance, or nil if none.
func (s *DeployState) ActiveInstance() *Instance {
	if s == nil {
		return nil
	}
	switch s.Mode {
	case ModeBlueGreen:
		if s.ActiveSlot == "" || s.Slots == nil {
			return nil
		}
		return s.Slots[s.ActiveSlot]
	case ModeRolling:
		// No single "active" replica for rolling; return nil. Callers iterate
		// Replicas themselves.
		return nil
	}
	return nil
}

// --- persistence -----------------------------------------------------------

// statePath returns the on-disk path for an app's DeployState, mirroring the
// health package's ~/.phelix/apps/<AppName>/ layout.
func statePath(appName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".phelix", "apps", appName, "deploy.json"), nil
}

// Store persists the DeployState to disk atomically (write tmp + rename).
func Store(s *DeployState) error {
	if s == nil || s.AppName == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: cannot store state without an app name")
	}
	path, err := statePath(s.AppName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: create state dir")
	}

	s.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads a DeployState for the given app. It returns an error wrapping
// os.ErrNotExist when no state file exists yet.
func Load(appName string) (*DeployState, error) {
	path, err := statePath(appName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s DeployState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "deploy: decode %s", path)
	}
	return &s, nil
}

// LoadZeroDowntime returns the app's state when it is managed by a
// zero-downtime strategy (blue-green/rolling), and nil otherwise — no state
// file, an unreadable one, or a mode outside those two all mean "classic".
//
// It is the single definition of "is this app deploy-managed", shared by the
// CLI lifecycle commands and the monitor daemon's remote-command executor.
// Those two disagreeing is what let a backend-issued start/restart kill a
// serving instance and try to rebind the proxy-owned public port.
func LoadZeroDowntime(appName string) *DeployState {
	s, err := Load(appName)
	if err != nil || s == nil {
		return nil
	}
	if s.Mode != ModeBlueGreen && s.Mode != ModeRolling {
		return nil
	}
	return s
}

// LoadOrInit returns the existing state for the app, or a freshly initialised
// one with the given mode and public port when none exists yet.
func LoadOrInit(appName string, mode Mode, publicPort int) (*DeployState, error) {
	if s, err := Load(appName); err == nil {
		return s, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	s := &DeployState{
		AppName:    appName,
		Mode:       mode,
		PublicPort: publicPort,
	}
	if mode == ModeBlueGreen {
		s.Slots = map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, Status: "stopped"},
			SlotGreen: {Slot: SlotGreen, Status: "stopped"},
		}
	} else {
		s.Replicas = make(map[string]*Instance)
	}
	return s, nil
}

// Remove deletes the deploy state file for an app.
func Remove(appName string) error {
	path, err := statePath(appName)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Dir(path)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RemoveState deletes only deploy.json, leaving versions.json and the build
// metadata that share the app's data directory intact. It is the strategy
// migration to classic: the deployment record goes away, the rollback history
// stays.
func RemoveState(appName string) error {
	path, err := statePath(appName)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
