package deploy

import (
	"context"
	"fmt"
	"os"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// RollbackPlan is the read-only description of what a rollback WOULD do for an
// app. PlanRollback builds it from the same resolution helpers the real
// rollback path uses (CurrentVersion, VersionPaths, Load, …) and describes the
// exact steps ExecuteRollback / the CLI classic path would take — without
// performing any of them. `rollback --dry-run` renders it; the normal rollback
// keeps executing the very flow the plan mirrors.
type RollbackPlan struct {
	AppName        string
	CurrentVersion int
	TargetVersion  int

	// Target metadata (optional — zero values render as "—").
	TargetTag     string
	TargetCommit  string
	TargetBuiltAt time.Time
	TargetSize    int64

	// Current metadata for the Changes section.
	CurrentTag    string
	CurrentCommit string
	CurrentSize   int64

	// BinaryPath is the validated target artifact (builds/vN/binary).
	BinaryPath string
	// EnvSnapshotAvailable reports whether env/vN.enc exists on disk. Only
	// meaningful for zero-downtime strategies; classic never restores one.
	EnvSnapshotAvailable bool
	// EnvSummary is the human one-liner for the Environment row.
	EnvSummary string

	// Strategy is "blue-green", "rolling" or "classic".
	Strategy string
	// Replicas is the rolling replica count (0 for non-rolling).
	Replicas int

	// Blue-green traffic transition.
	PublicPort  int
	CurrentSlot string
	TargetSlot  string
	CurrentPort int // active instance's internal port, 0 when unknown

	// HealthCheck describes the tier the rollback will use. Zero-downtime only.
	HealthCheck string
	// Downtime is true when the classic rollback stops a running instance.
	Downtime bool

	// Steps mirror the real executor's step order (see ExecuteRollback and
	// rollbackClassic).
	Steps []string
	// Warnings are non-fatal observations; a real rollback would still start.
	Warnings []string
}

// RollbackPlanInput carries what the caller already resolved before planning.
type RollbackPlanInput struct {
	// AppID is the managed app ID (used to look up the health-tier config).
	AppID string
	// Target is the resolved concrete version (caller already validated it).
	Target int
	// ClassicRunning / ClassicPort / ClassicDestBin describe the classic
	// rollback's start/stop targets (appInfo fields).
	ClassicRunning bool
	ClassicPort    int
	ClassicDestBin string
}

// PlanRollback resolves everything a rollback needs and returns the plan.
// It performs only reads (stat/read of ~/.phelix files, one advisory lock
// probe, one proxy ping) and never mutates app, process, proxy, version or
// audit state.
func PlanRollback(appName string, in RollbackPlanInput) (*RollbackPlan, error) {
	cur, _ := CurrentVersion(appName)
	if cur > 0 && in.Target == cur {
		return nil, phelixerr.Newf(phelixerr.CodeRollbackTargetNotFound,
			"deploy: already running v%d; nothing to roll back to", cur)
	}

	p := &RollbackPlan{AppName: appName, CurrentVersion: cur, TargetVersion: in.Target}

	vf, err := LoadVersions(appName)
	if err != nil {
		return nil, err
	}
	if !versionExists(vf, in.Target) {
		return nil, phelixerr.Newf(phelixerr.CodeVersionNotFound,
			"deploy: version v%d does not exist for %q; available: %s", in.Target, appName, formatAvailableVersions(vf))
	}
	if cm := findVersionMeta(vf, cur); cm != nil {
		p.CurrentTag, p.CurrentCommit, p.CurrentSize = cm.Tag, cm.GitCommit, cm.SizeBytes
	}
	if tm := findVersionMeta(vf, in.Target); tm != nil {
		p.TargetTag, p.TargetCommit, p.TargetBuiltAt, p.TargetSize = tm.Tag, tm.GitCommit, tm.BuiltAt, tm.SizeBytes
	}

	// Same artifact validation the real rollback does via ExistingVersionSource
	// → VersionPaths: binary missing is a hard error, snapshot missing is not.
	bin, env, err := VersionPaths(appName, in.Target)
	if err != nil {
		return nil, err
	}
	p.BinaryPath = bin

	state, stateErr := Load(appName)
	switch {
	case stateErr != nil && !os.IsNotExist(stateErr):
		return nil, stateErr
	case stateErr != nil || state == nil || state.Mode == "":
		p.planClassic(in)
	case state.Mode == ModeBlueGreen:
		p.planBlueGreen(state, in, env)
		p.checkProxyReachability()
	case state.Mode == ModeRolling:
		p.planRolling(state, in, env)
		p.checkProxyReachability()
	default:
		return nil, phelixerr.Newf(phelixerr.CodeRollbackFailed,
			"deploy: rollback unsupported for mode %q", state.Mode)
	}

	p.warnIfDeployLocked(appName)
	p.warnOnDistance(cur, in.Target)
	return p, nil
}

// planClassic mirrors rollbackClassic in cmd/rollback.go: stop (if running) →
// copy binary → start → promote. No env snapshot and no deploy health checks
// on this path.
func (p *RollbackPlan) planClassic(in RollbackPlanInput) {
	p.Strategy = "classic"
	p.Downtime = in.ClassicRunning
	p.HealthCheck = "none (classic rollback starts without deploy health checks)"
	p.EnvSummary = "live environment (classic rollback does not restore versioned snapshots)"
	if in.ClassicRunning {
		p.Steps = append(p.Steps, "Stop the current instance")
	}
	p.Steps = append(p.Steps,
		fmt.Sprintf("Copy the v%d binary to %s", p.TargetVersion, in.ClassicDestBin),
		fmt.Sprintf("Start the application on port %d", in.ClassicPort),
		fmt.Sprintf("Promote v%d as current (versions.json + current symlink)", p.TargetVersion),
	)
}

// planBlueGreen mirrors ExecuteRollback → BlueGreen.Deploy: artifact + env →
// start inactive slot → health → proxy switch → promote → drain old slot.
func (p *RollbackPlan) planBlueGreen(state *DeployState, in RollbackPlanInput, env string) {
	p.Strategy = string(ModeBlueGreen)
	p.PublicPort = state.PublicPort
	p.CurrentSlot = state.ActiveSlot
	p.TargetSlot = state.InactiveSlot()
	if ai := state.ActiveInstance(); ai != nil {
		p.CurrentPort = ai.Port
	}
	p.envNote(env)
	p.HealthCheck = describeRollbackHealth(in.AppID)
	grace := state.GraceSeconds
	if grace <= 0 {
		grace = int(DefaultGracePeriod / time.Second)
	}

	p.Steps = append(p.Steps,
		"Ensure the proxy daemon is running",
		fmt.Sprintf("Load the v%d artifact (%s)", p.TargetVersion, p.BinaryPath),
	)
	if p.EnvSnapshotAvailable {
		p.Steps = append(p.Steps, fmt.Sprintf("Restore the v%d environment snapshot (env/v%d.enc)", p.TargetVersion, p.TargetVersion))
	} else {
		p.Steps = append(p.Steps, "Start the new instance with the current environment (no snapshot to restore)")
	}
	p.Steps = append(p.Steps,
		fmt.Sprintf("Start the new instance on the inactive slot %s (internal port assigned at startup)", p.TargetSlot),
		"Run health checks against the new instance",
	)
	if state.ActiveSlot == "" {
		p.Steps = append(p.Steps, fmt.Sprintf("Enrol the app with the proxy on public port %d → slot %s", p.PublicPort, p.TargetSlot))
	} else {
		p.Steps = append(p.Steps, fmt.Sprintf("Switch proxy traffic on public port %d from slot %s to slot %s", p.PublicPort, p.CurrentSlot, p.TargetSlot))
	}
	p.Steps = append(p.Steps,
		fmt.Sprintf("Promote v%d as current (versions.json + current symlink)", p.TargetVersion),
	)
	if state.ActiveSlot != "" {
		p.Steps = append(p.Steps, fmt.Sprintf("Drain and stop the old slot %s instance (grace %ds)", p.CurrentSlot, grace))
	}
}

// planRolling mirrors ExecuteRollback → Rolling.Deploy: build once, then for
// each replica (in index order) start replacement alongside → health-check it
// → swap proxy membership → drain old; promote after all replicas serve.
func (p *RollbackPlan) planRolling(state *DeployState, in RollbackPlanInput, env string) {
	p.Strategy = string(ModeRolling)
	p.PublicPort = state.PublicPort
	p.Replicas = len(state.Replicas)
	if p.Replicas < 1 {
		p.Replicas = 1 // mirror ExecuteRollback's minimum
	}
	p.envNote(env)
	p.HealthCheck = describeRollbackHealth(in.AppID)

	p.Steps = append(p.Steps,
		"Ensure the proxy daemon is running",
		fmt.Sprintf("Load the v%d artifact (%s)", p.TargetVersion, p.BinaryPath),
	)
	if p.EnvSnapshotAvailable {
		p.Steps = append(p.Steps, fmt.Sprintf("Restore the v%d environment snapshot (env/v%d.enc)", p.TargetVersion, p.TargetVersion))
	} else {
		p.Steps = append(p.Steps, "Start the replacements with the current environment (no snapshot to restore)")
	}

	keys := replicaIndices(state.Replicas)
	if len(keys) == 0 {
		keys = []string{"0"}
	}
	for _, key := range keys {
		if inst := state.Replicas[key]; inst != nil && inst.PID > 0 {
			p.Steps = append(p.Steps, fmt.Sprintf(
				"Replace replica %s: start a replacement alongside pid %d, health-check it, swap proxy membership, drain the old instance",
				key, inst.PID))
		} else {
			p.Steps = append(p.Steps, fmt.Sprintf(
				"Replica %s: start an instance, health-check it, add it to the proxy rotation", key))
		}
	}
	p.Steps = append(p.Steps, fmt.Sprintf("Promote v%d as current (versions.json + current symlink)", p.TargetVersion))
}

// envNote records the target snapshot status. A missing snapshot is a warning,
// not a blocker — that matches the real rollback, where VersionPaths returns
// an empty env path and the instance simply starts without an overlay.
func (p *RollbackPlan) envNote(env string) {
	if env == "" {
		p.EnvSnapshotAvailable = false
		p.EnvSummary = fmt.Sprintf("v%d snapshot missing", p.TargetVersion)
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"v%d environment snapshot is missing; the rollback will start without restored env (the current environment applies)",
			p.TargetVersion))
		return
	}
	p.EnvSnapshotAvailable = true
	p.EnvSummary = fmt.Sprintf("v%d snapshot available", p.TargetVersion)
}

func (p *RollbackPlan) warnIfDeployLocked(appName string) {
	// Only probe when the lock file already exists: TryLoadLock opens with
	// O_CREATE and dry-run must never create the file.
	lp, err := lockPath(appName)
	if err != nil {
		return
	}
	if _, statErr := os.Stat(lp); statErr != nil {
		return
	}
	if holder, lockErr := TryLoadLock(appName); lockErr == nil && holder != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"A %q operation (pid %d) holds the deploy lock; the rollback will be rejected until it finishes",
			holder.Operation, holder.PID))
	}
}

func (p *RollbackPlan) warnOnDistance(cur, target int) {
	if cur > 0 {
		if d := cur - target; d >= 5 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("Target version is %d versions behind current", d))
		}
	}
}

// checkProxyReachability pings the proxy daemon read-only. The real rollback
// starts the daemon (EnsureDaemon) before switching, so an unreachable daemon
// is worth surfacing — but starting one here would be a mutation.
func (p *RollbackPlan) checkProxyReachability() {
	socket, err := proxy.DefaultSocketPath()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := proxy.NewClient(socket).Ping(ctx); err != nil {
		p.Warnings = append(p.Warnings, "Proxy daemon is not reachable; the rollback will start it first (phelix proxy)")
	}
}

// describeRollbackHealth renders the tier the rollback will use, from the same
// EffectiveDeployTier config the real path resolves. Tiers that depend on
// probing a not-yet-started instance (auto without a path) are reported as
// such instead of guessed.
func describeRollbackHealth(appID string) string {
	provider := DefaultHealthProvider()
	cfg := provider(appID)
	if cfg == nil {
		return "auto (no endpoint configured; selected when the instance starts)"
	}
	switch cfg.Mode {
	case health.TierModeNone:
		return health.Tier3None.String()
	case health.TierModeTCPOnly:
		return health.Tier3TCP.String()
	case health.TierModeHTTP:
		if cfg.Path != "" {
			return fmt.Sprintf("Tier 1 (%s, 2xx required)", cfg.Path)
		}
		return health.Tier2HTTPAny.String()
	default: // auto
		if cfg.Path != "" {
			return fmt.Sprintf("Tier 1 (%s, 2xx required)", cfg.Path)
		}
		return "auto (Tier 2/3; selected when the instance starts)"
	}
}

func findVersionMeta(vf *VersionsFile, ver int) *VersionMeta {
	for i := range vf.Versions {
		if vf.Versions[i].Version == ver {
			return &vf.Versions[i]
		}
	}
	return nil
}
