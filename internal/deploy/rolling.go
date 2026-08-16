package deploy

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// Rolling holds the dependencies for a rolling deploy over N replicas.
type Rolling struct {
	AppName    string
	AppID      string
	PublicPort int
	Replicas   int // desired replica count
	ExtraArgs  []string

	Builder        Builder
	Source         BuildSource
	Launcher       InstanceLauncher
	ProxyClient    ProxyClient
	HealthProvider HealthConfigProvider
	Logger         Logger
	Notifier       Notifier
	InFlight       InFlightProvider
	GracePeriod    time.Duration
}

// Deploy restarts replicas one at a time. For each replica index 0..N-1 it:
//
//  1. Stops the current instance (if any) — taking it out of rotation.
//  2. Builds the new binary.
//  3. Starts a replacement instance.
//  4. Runs the tiered health check on it.
//  5. Re-adds it to the proxy's backend set.
//
// It never takes more than one replica down at a time. A health-check failure
// on a replacement aborts the whole rolling deploy with a non-nil error: the
// failing replacement is killed and the remaining replicas are left as-is, so
// at least N-1 replicas stay serving.
func (r *Rolling) Deploy(ctx context.Context) error {
	log := r.logger()
	grace := r.GracePeriod
	if grace <= 0 {
		grace = DefaultGracePeriod
	}
	if r.Replicas < 1 {
		r.Replicas = 1
	}

	src := r.Source
	if src == nil && r.Builder != nil {
		src = BuilderSource(r.Builder)
	}
	if src == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: no BuildSource or Builder configured")
	}

	state, err := LoadOrInit(r.AppName, ModeRolling, r.PublicPort)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "failed to load deploy state")
	}
	state.AppID = r.AppID
	state.Replicas = ensureReplicaMap(state.Replicas, r.Replicas)

	log.Stepf("rolling deploy for %s: %d replicas", r.AppName, r.Replicas)

	// Build/prepare once — FreshBuildSource must not create a new version per replica.
	log.Stepf("%s", src.Describe())
	binaryPath, envPath, err := src.Build(ctx)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeBuildFailed, err, "prepare deploy artifact")
	}
	envOverlay, err := EnvOverlayFromSnapshot(envPath, r.AppID)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "env snapshot")
	}

	targetVer := targetVersionFromSource(src)

	tierCfg := r.healthCfg()
	tier := health.SelectTier(tierCfg, firstPortAddr(state), nil)
	log.Infof("health tier selected: %s", tier)
	if tier != health.Tier1HTTPPath {
		msg := fmt.Sprintf("⚠ No health endpoint configured for %s — using %s.\n"+
			"  Add one with: phelix health set %s --path /your-health-path",
			r.AppName, tier, r.AppName)
		log.Warnf("%s", msg)
		if r.Notifier != nil {
			_ = r.Notifier.Notify(ctx, msg)
		}
	}

	// Iterate over replica indices in order.
	indices := replicaIndices(state.Replicas)
	for _, key := range indices {
		if err := r.rollOne(ctx, state, binaryPath, envOverlay, targetVer, key, tier, tierCfg, grace); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeDeployFailed, err, "rolling deploy failed at replica %s", key)
		}
	}

	if state.Health == nil {
		state.Health = &HealthSummary{}
	}
	state.Health.Tier = int(tier)
	state.Health.TierLabel = tier.String()
	state.Health.HealthyAt = time.Now()
	if ver := targetVersionFromSource(src); ver > 0 {
		state.ActiveVersion = ver
		if err := PromoteVersion(r.AppName, ver, string(ModeRolling)); err != nil {
			log.Warnf("failed to promote version v%d: %v", ver, err)
		}
	}
	_ = Store(state)

	log.Successf("rolling deploy of %s complete: %d replicas updated", r.AppName, len(indices))
	return nil
}

// rollOne performs the stop→build→start→health→re-add cycle for one replica.
func (r *Rolling) rollOne(ctx context.Context, state *DeployState, binaryPath string, envOverlay []string, targetVer int, key string, tier health.Tier, tierCfg *health.DeployTierConfig, grace time.Duration) error {
	log := r.logger()

	// 1. Stop current instance for this slot (remove from rotation first).
	cur := state.Replicas[key]
	if cur != nil && cur.PID > 0 {
		log.Stepf("replica %s: stopping current instance (pid %d)", key, cur.PID)
		stopByPID(ctx, cur.PID, grace, r.inFlight(r.AppName))
		cur.PID = 0
		cur.Status = "stopped"
	}

	// 2. Start replacement (artifact prepared once for the whole roll).
	log.Stepf("replica %s: starting from %s", key, binaryPath)
	proc, port, err := r.Launcher(ctx, binaryPath, envOverlay)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "start replica %s", key)
	}
	inst := &Instance{
		Slot:       key,
		PID:        proc.PID(),
		Port:       port,
		BinaryPath: binaryPath,
		StartedAt:  time.Now(),
		Status:     "running",
		Version:    targetVer,
	}
	state.Replicas[key] = inst
	_ = Store(state)

	// 4. Health check.
	if err := health.WaitForHealthy(ctx, tier, tierCfg, hostPort(port), proc.PID(), nil); err != nil {
		// Kill the unhealthy replacement; leave other replicas serving.
		stopByPID(ctx, proc.PID(), grace, 0)
		inst.Status = "failed"
		inst.PID = 0
		_ = Store(state)
		return phelixerr.Wrapf(phelixerr.CodeHealthCheckFailed, err, "replica %s failed health check; other replicas left serving", key)
	}

	// 5. Re-add to the proxy backend set: primary = the lowest-index healthy
	//    replica, backends = all healthy replicas.
	primary, backends := healthyTargets(state.Replicas)
	if r.ProxyClient != nil {
		if err := r.ProxyClient.Switch(ctx, r.AppName, primary, backends...); err != nil {
			// Non-fatal: the replica is healthy and running; the proxy just
			// hasn't been told. Surface as a warning.
			log.Warnf("replica %s: proxy update failed: %v (instance still running)", key, err)
		}
	}
	log.Successf("replica %s: healthy and in rotation (pid %d, port %d)", key, inst.PID, inst.Port)
	return nil
}

func (r *Rolling) logger() Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return &nopLogger{}
}

func (r *Rolling) healthCfg() *health.DeployTierConfig {
	if r.HealthProvider != nil {
		return r.HealthProvider(r.AppID)
	}
	return nil
}

func (r *Rolling) inFlight(appName string) int64 {
	if r.InFlight == nil {
		return 0
	}
	return r.InFlight(appName)
}

// --- replica helpers -------------------------------------------------------

// ensureReplicaMap returns a replica map sized to n, preserving any existing
// instances for indices < n and dropping indices >= n.
func ensureReplicaMap(existing map[string]*Instance, n int) map[string]*Instance {
	out := make(map[string]*Instance, n)
	for i := 0; i < n; i++ {
		key := strconv.Itoa(i)
		if inst, ok := existing[key]; ok {
			out[key] = inst
		} else {
			out[key] = &Instance{Slot: key, Status: "stopped"}
		}
	}
	return out
}

func replicaIndices(m map[string]*Instance) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		ai, _ := strconv.Atoi(out[i])
		aj, _ := strconv.Atoi(out[j])
		return ai < aj
	})
	return out
}

// healthyTargets returns (primary, backends) over all healthy replicas. The
// primary is the lowest-index healthy replica; backends includes the primary.
func healthyTargets(replicas map[string]*Instance) (proxy.Target, []proxy.Target) {
	indices := replicaIndices(replicas)
	var backends []proxy.Target
	var primary proxy.Target
	for _, k := range indices {
		inst := replicas[k]
		if inst == nil || inst.Status != "running" || inst.Port == 0 {
			continue
		}
		t := proxy.Target{Host: hostPort(inst.Port), Label: "replica-" + k}
		backends = append(backends, t)
		if primary.Host == "" {
			primary = t
		}
	}
	if primary.Host == "" {
		primary = proxy.Target{Host: "127.0.0.1:0", Label: "none"}
	}
	return primary, backends
}

// firstPortAddr returns the host:port of the first replica with a port, for
// tier auto-selection. Returns a non-routable address when none is running.
func firstPortAddr(state *DeployState) string {
	if state == nil || state.Replicas == nil {
		return "127.0.0.1:0"
	}
	for _, k := range replicaIndices(state.Replicas) {
		if inst := state.Replicas[k]; inst != nil && inst.Port != 0 {
			return hostPort(inst.Port)
		}
	}
	return "127.0.0.1:0"
}
