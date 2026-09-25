package deploy

import (
	"context"
	"strconv"
	"time"

	"github.com/abdorrahmani/phelix/internal/health"
)

// Deployment state resync: rebuilding a Snapshot for a consumer that did NOT
// run the deployment (the monitor daemon re-reporting state to the backend
// after a reconnect, a dashboard refresh, or a CLI restart). It reads the same
// deploy.json the deployment flows write, so there is no second source of
// truth — only a second reader.

// SnapshotForApp rebuilds the deployment topology of an app from persisted
// state. Returns nil when the app has no zero-downtime deployment.
//
// It reports last-known health rather than probing: an instance is healthy only
// when its process is alive AND the deployment recorded that the app passed its
// health window (deploy.json's health.healthy_at). A dead or identity-
// unverifiable process is never reported healthy.
//
// pc is optional; when non-nil the proxy daemon is queried so routing describes
// what the daemon actually serves rather than being inferred from app status.
func SnapshotForApp(ctx context.Context, appName, appID string, pc ProxyClient) *Snapshot {
	state, err := Load(appName)
	if err != nil || state == nil || state.Mode == "" {
		return nil
	}

	s := &Snapshot{
		AppID:          appID,
		AppName:        appName,
		DeploymentID:   state.LastDeploymentID,
		RequestID:      state.LastRequestID,
		Strategy:       string(state.Mode),
		Runtime:        state.Runtime,
		CurrentVersion: versionLabel(state.ActiveVersion),
		TargetVersion:  versionLabel(state.ActiveVersion),
		ActiveSlot:     state.ActiveSlot,
		UpdatedAt:      time.Now(),
	}
	if s.AppID == "" {
		s.AppID = state.AppID
	}
	if state.Health != nil {
		s.Health = &HealthState{
			Tier:          state.Health.Tier,
			TierLabel:     state.Health.TierLabel,
			LastHealthyAt: state.Health.HealthyAt,
		}
	}

	healthOf := func(inst *Instance) string {
		switch {
		case inst == nil:
			return HealthUnknown
		case inst.Status == "failed":
			return HealthUnhealthy
		case inst.Status == "starting", inst.Status == "replacing":
			return HealthPending
		case InstanceAlive(inst) && state.Health != nil && !state.Health.HealthyAt.IsZero():
			return HealthHealthy
		}
		return HealthUnknown
	}
	healthyAt := func(inst *Instance) time.Time {
		if state.Health == nil || healthOf(inst) != HealthHealthy {
			return time.Time{}
		}
		return state.Health.HealthyAt
	}

	for _, name := range []string{SlotBlue, SlotGreen} {
		inst := state.Slots[name]
		if inst == nil {
			continue
		}
		s.Slots = append(s.Slots, SlotState{
			Slot:         name,
			Version:      versionLabel(inst.Version),
			Status:       instanceStatus(inst),
			Health:       healthOf(inst),
			InternalPort: inst.Port,
			PID:          inst.PID,
			Active:       name == state.ActiveSlot,
			StartedAt:    inst.StartedAt,
			HealthyAt:    healthyAt(inst),
			Image:        dockerImageOf(inst),
			ContainerID:  inst.ContainerID,
		})
	}

	for _, key := range replicaIndices(state.Replicas) {
		inst := state.Replicas[key]
		idx, cerr := strconv.Atoi(key)
		if inst == nil || cerr != nil {
			continue
		}
		status := instanceStatus(inst)
		h := healthOf(inst)
		s.Replicas = append(s.Replicas, ReplicaState{
			ID:           replicaID(idx),
			Index:        idx,
			Version:      versionLabel(inst.Version),
			Status:       status,
			Health:       h,
			InternalPort: inst.Port,
			PID:          inst.PID,
			StartedAt:    inst.StartedAt,
			HealthyAt:    healthyAt(inst),
			Image:        dockerImageOf(inst),
			ContainerID:  inst.ContainerID,
		})
		if inst.PID > 0 {
			s.ReplicasCurrent++
		}
		// Readiness is verified, not trusted: unlike the live Tracker (which
		// reports what the running deployment just observed), a resync reads
		// records written by an earlier process. deploy.json can name a replica
		// as running whose process died since, so a replica only counts as
		// ready when its recorded PID is alive AND still resolves to the binary
		// recorded for it.
		if status == "running" && InstanceAlive(inst) {
			s.ReplicasReady++
			if h == HealthHealthy {
				s.ReplicasHealthy++
			}
		}
	}
	if state.Mode == ModeRolling {
		s.ReplicasDesired = len(state.Replicas)
	}

	// Lifecycle comes from the live OS lock, not the advisory OpLock mirror in
	// deploy.json. A process crash releases flock but cannot run the deferred
	// mirror cleanup, so trusting the persisted field would report in_progress
	// forever after restart.
	holder, lockErr := TryLoadLock(appName)
	switch {
	case lockErr == nil && holder != nil:
		s.Phase = PhaseIdle
		s.Status = StatusInProgress
	case InstanceAlive(state.ServingInstance()):
		s.Phase = PhaseCompleted
		s.Status = StatusSucceeded
	default:
		s.Phase = PhaseIdle
		s.Status = StatusIdle
	}

	s.Proxy = proxyStateOf(ctx, pc, appName, state.PublicPort)
	return s
}

// proxyStateOf asks the proxy daemon what it actually routes for this app.
// Returns nil when the daemon cannot be consulted, which the backend must read
// as "unknown" — never as "proxy disabled".
func proxyStateOf(ctx context.Context, pc ProxyClient, appName string, publicPort int) *ProxyState {
	if pc == nil {
		return nil
	}
	sts, err := pc.Status(ctx, appName)
	if err != nil {
		return nil
	}
	for _, st := range sts {
		if st.AppName != appName {
			continue
		}
		ps := &ProxyState{
			Enabled:     true,
			PublicPort:  st.PublicPort,
			TargetLabel: st.Primary.Label,
			Upstreams:   upstreamHosts(st.Primary, st.Backends),
			InFlight:    st.InFlight,
		}
		if ps.PublicPort == 0 {
			ps.PublicPort = publicPort
		}
		if _, port, perr := health.MustParseHostPort(st.Primary.Host); perr == nil {
			ps.TargetInternalPort = port
		}
		return ps
	}
	// The daemon answered and does not know this app: it genuinely is not
	// proxied right now.
	return &ProxyState{PublicPort: publicPort}
}
