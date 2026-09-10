package health

import "time"

// The canonical agent -> backend health model.
//
// AppHealthSnapshot is the ONE shape the CLI reports health with. It is a
// complete statement of one application's health at one instant, built from the
// same two sources the CLI itself reads: the persisted configuration
// (health.json) and the daemon's in-memory observations. There is no third
// store and no second serialization path.
//
// Configuration and observation are kept apart by construction:
// EndpointHealthSnapshot.Config is what the operator asked for, and .Result plus
// the surrounding counters are what the daemon saw. Result is nil until the
// endpoint has actually been probed, so "not checked yet" can never be read as
// "down".
//
// Because every message is a complete snapshot, the backend reconciles by
// replacement — which makes create, update, delete and reconnect-recovery the
// same operation, idempotent, and independent of message ordering. See
// docs/health-backend-contract.md.

// Application-level rollup values. They share the uppercase vocabulary the
// per-endpoint statuses already use (UP / DOWN / TIMEOUT, and UNKNOWN for a
// never-probed endpoint — see internal/health/display.go).
const (
	// StatusUnknown means no endpoint has produced a result yet.
	StatusUnknown = "UNKNOWN"
	// StatusUp means every probed endpoint is UP.
	StatusUp = "UP"
	// StatusDown means no probed endpoint is UP, and at least one was probed.
	StatusDown = "DOWN"
	// StatusDegraded means some but not all probed endpoints are UP.
	StatusDegraded = "DEGRADED"
)

// EndpointHealthSnapshot is one endpoint's configuration paired with what the
// daemon last observed for it.
type EndpointHealthSnapshot struct {
	// Config carries the endpoint's stable ID along with the operator-declared
	// settings. Config.ID is the identity the backend keys on.
	Config HealthCheckConfig
	// Result is the last completed probe, or nil when the endpoint has never
	// been probed.
	Result *HealthCheckResult

	ConsecutiveFailures int
	LastSuccess         time.Time
	LastFailure         time.Time
	BackoffLevel        int
	NextRestart         time.Time
	CrashCount24h       int
}

// AppHealthSnapshot is the complete health state of one application.
type AppHealthSnapshot struct {
	AppID   string
	AppName string
	Enabled bool
	// Status is the rollup over probed endpoints: one of the Status* constants.
	Status    string
	UpdatedAt time.Time
	// DeployTier is the effective zero-downtime-deploy probe configuration —
	// the same value the deploy flow resolves, not the raw persisted block.
	DeployTier *DeployTierConfig
	// EndpointsTotal is len(Endpoints); EndpointsUp counts endpoints whose last
	// result was UP. Both are derived and reported for convenience.
	EndpointsTotal int
	EndpointsUp    int
	// Endpoints is the COMPLETE set configured for this app, in stable name
	// order. An empty slice means the app has a health configuration with no
	// endpoints — the backend must then hold none either.
	Endpoints []EndpointHealthSnapshot
}

// SnapshotForApp builds the current health state of one application.
//
// It returns nil when the app has no health configuration at all: absence on the
// wire means "health is not configured", never "unhealthy". A configuration that
// exists but lists zero endpoints DOES produce a snapshot (with no endpoints), so
// removing the last endpoint still reaches the backend instead of leaving an
// orphan there.
func (d *Daemon) SnapshotForApp(appID, appName string) *AppHealthSnapshot {
	config := d.configManager.GetConfig(appID)
	if config == nil {
		return nil
	}
	if appName == "" {
		appName = config.AppName
	}

	snap := &AppHealthSnapshot{
		AppID:      appID,
		AppName:    appName,
		Enabled:    config.Enabled,
		UpdatedAt:  time.Now(),
		DeployTier: config.EffectiveDeployTier(),
		Endpoints:  make([]EndpointHealthSnapshot, 0, len(config.Endpoints)),
	}

	d.mu.RLock()
	states := d.endpointStates[appID]
	for _, name := range sortedEndpointNames(config.Endpoints) {
		ep := config.Endpoints[name]
		if ep == nil {
			continue
		}
		e := EndpointHealthSnapshot{Config: *ep}
		if state := states[name]; state != nil {
			if r := state.LastResult; r != nil {
				// Copied so the caller cannot observe a later replacement
				// mid-serialization.
				result := *r
				e.Result = &result
			}
			e.ConsecutiveFailures = state.ConsecutiveFailures
			e.LastSuccess = state.LastSuccessTime
			e.LastFailure = state.LastFailTime
			e.BackoffLevel = state.BackoffLevel
			e.NextRestart = state.NextRestartTime
			e.CrashCount24h = crashesWithin24h(state.CrashHistory)
		}
		snap.Endpoints = append(snap.Endpoints, e)
	}
	d.mu.RUnlock()

	checked := 0
	for _, e := range snap.Endpoints {
		if e.Result == nil {
			continue
		}
		checked++
		if e.Result.Status == StatusUp {
			snap.EndpointsUp++
		}
	}
	snap.EndpointsTotal = len(snap.Endpoints)
	snap.Status = rollupStatus(checked, snap.EndpointsUp)
	return snap
}

// EndpointList returns the snapshot's endpoints, tolerating a nil snapshot so
// callers can render "no health configured" as an empty list.
func (s *AppHealthSnapshot) EndpointList() []EndpointHealthSnapshot {
	if s == nil {
		return nil
	}
	return s.Endpoints
}

// rollupStatus reduces per-endpoint results to one application status. checked is
// how many endpoints have produced a result; up is how many of those are UP.
func rollupStatus(checked, up int) string {
	switch {
	case checked == 0:
		return StatusUnknown
	case up == checked:
		return StatusUp
	case up == 0:
		return StatusDown
	default:
		return StatusDegraded
	}
}

// crashesWithin24h counts auto-restarts in the last 24h. The stored history is
// only pruned when a restart triggers, so it is filtered on read too.
func crashesWithin24h(history []time.Time) int {
	now := time.Now()
	n := 0
	for _, t := range history {
		if now.Sub(t) < 24*time.Hour {
			n++
		}
	}
	return n
}
