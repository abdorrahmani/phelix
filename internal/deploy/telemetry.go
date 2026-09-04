package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// Deployment telemetry: an observation layer over the deployment flows in this
// package. It reports what a deployment is doing to whoever is listening (the
// gRPC backend reporter in production, a recorder in tests) without changing
// any deployment decision. Every value it publishes is read from state this
// package already owns — deploy.json, versions.json, the health config and the
// proxy facts the flow just established — so telemetry can never disagree with
// the deployment.
//
// A nil *Tracker is a valid, silent tracker: all methods are no-ops. That is
// what keeps offline/unauthenticated deploys byte-for-byte unchanged.

// Deployment phases. They describe where in its lifecycle a deployment is;
// there is deliberately no progress percentage, because the flows have no
// meaningful notion of "fraction complete".
const (
	PhaseIdle        = "idle"
	PhaseBuilding    = "building"
	PhaseStarting    = "starting"
	PhaseHealthCheck = "health_check"
	PhaseSwitching   = "switching"
	PhasePromoting   = "promoting"
	PhaseDraining    = "draining"
	PhaseCompleted   = "completed"
	PhaseFailed      = "failed"
	PhaseCancelled   = "cancelled"
)

// Deployment statuses. status is the terminal-or-not view of a deployment,
// phase is where it currently is.
const (
	StatusIdle       = "idle"
	StatusInProgress = "in_progress"
	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"
	StatusCancelled  = "cancelled"
)

// Deployment lifecycle event names.
const (
	EventStarted            = "deployment.started"
	EventBuilding           = "deployment.building"
	EventInstanceStarted    = "deployment.instance_started"
	EventHealthCheckStarted = "deployment.health_check_started"
	EventInstanceHealthy    = "deployment.instance_healthy"
	EventProxySwitching     = "deployment.proxy_switching"
	EventProxySwitched      = "deployment.proxy_switched"
	EventInstanceDraining   = "deployment.instance_draining"
	EventInstanceStopped    = "deployment.instance_stopped"

	EventReplicaStarted            = "deployment.replica_started"
	EventReplicaHealthCheckStarted = "deployment.replica_health_check_started"
	EventReplicaHealthy            = "deployment.replica_healthy"
	EventReplicaReplaced           = "deployment.replica_replaced"
	EventReplicaDraining           = "deployment.replica_draining"
	EventReplicaStopped            = "deployment.replica_stopped"

	EventCompleted = "deployment.completed"
	EventFailed    = "deployment.failed"
	EventCancelled = "deployment.cancelled"
)

// StrategyClassic names the stop→build→start path. Blue-green and rolling
// reuse the existing Mode values, so telemetry never invents a strategy name
// the rest of the package does not use.
const StrategyClassic = "classic"

// Instance/replica health as far as Phelix actually knows it.
const (
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
	HealthPending   = "pending"
	HealthUnknown   = "unknown"
)

// Failure describes why a deployment stopped, in terms of the existing
// structured error model: Code is a phelixerr.Code and Message is redacted.
type Failure struct {
	Code      string
	Message   string
	Retryable bool
}

// SlotState is one blue-green slot as recorded in deploy.json.
type SlotState struct {
	Slot         string
	Version      string
	Status       string
	Health       string
	InternalPort int
	PID          int
	Active       bool
	StartedAt    time.Time
	HealthyAt    time.Time
}

// ReplicaState is one rolling replica as recorded in deploy.json.
type ReplicaState struct {
	ID           string
	Index        int
	Version      string
	Status       string
	Health       string
	InternalPort int
	PID          int
	StartedAt    time.Time
	HealthyAt    time.Time
}

// ProxyState is what the deployment knows about the proxy daemon's routing for
// this app. It is only populated from facts the flow established (a successful
// enrol/switch) or a live daemon query — never inferred from app status.
type ProxyState struct {
	Enabled            bool
	PublicPort         int
	TargetLabel        string
	TargetInternalPort int
	Upstreams          []string
	InFlight           int64
}

// HealthState is the deploy-time health configuration and the tier actually
// selected for this deployment. No metric is computed for telemetry alone.
type HealthState struct {
	Mode          string
	Path          string
	Retries       int
	Interval      string
	Timeout       string
	Tier          int
	TierLabel     string
	LastHealthyAt time.Time
}

// Snapshot is the deployment topology of one app at one instant.
type Snapshot struct {
	AppID          string
	AppName        string
	DeploymentID   string
	Strategy       string
	Phase          string
	Status         string
	CurrentVersion string
	TargetVersion  string
	ActiveSlot     string
	Slots          []SlotState
	Replicas       []ReplicaState

	ReplicasDesired int
	ReplicasCurrent int
	ReplicasReady   int
	ReplicasHealthy int

	Proxy     *ProxyState
	Health    *HealthState
	Failure   *Failure
	StartedAt time.Time
	UpdatedAt time.Time
}

// Event is one deployment lifecycle transition. Snapshot always carries the
// full state at emission time so a consumer that missed earlier events can
// still reconstruct the topology.
type Event struct {
	AppID        string
	AppName      string
	DeploymentID string
	Event        string
	Strategy     string
	Phase        string
	Status       string

	CurrentVersion string
	TargetVersion  string

	Slot            string
	ReplicaID       string
	ReplicaIndex    int
	InternalPort    int
	PID             int
	ReplicasDesired int

	Message   string
	Failure   *Failure
	Snapshot  *Snapshot
	Timestamp time.Time
}

// Sink consumes deployment telemetry. Implementations must not block the
// deployment: the production sink queues the event and returns.
type Sink interface {
	Deployment(Event)
}

// Tracker records a deployment's lifecycle and publishes it to a Sink. A nil
// Tracker is valid and silent, which is what keeps deploys with telemetry
// disabled (offline, not logged in) identical to before.
type Tracker struct {
	mu sync.Mutex

	sink    Sink
	id      string
	appID   string
	appName string

	strategy string
	phase    string
	status   string

	currentVersion string
	targetVersion  string

	replicasDesired int

	state   *DeployState
	proxy   *ProxyState
	health  *HealthState
	failure *Failure

	// healthy records which instances passed their health window during THIS
	// deployment, keyed by slot name / replica index.
	healthy map[string]time.Time

	startedAt time.Time
}

// NewTracker returns a tracker for one deployment operation, or nil when no
// sink is configured (telemetry off). strategy is StrategyClassic or one of the
// existing Mode values.
func NewTracker(sink Sink, appID, appName, strategy string) *Tracker {
	if sink == nil {
		return nil
	}
	return &Tracker{
		sink:      sink,
		id:        newDeploymentID(),
		appID:     appID,
		appName:   appName,
		strategy:  strategy,
		phase:     PhaseIdle,
		status:    StatusIdle,
		healthy:   make(map[string]time.Time),
		startedAt: time.Now(),
	}
}

// DeploymentID is the identifier every event of this deployment carries.
func (t *Tracker) DeploymentID() string {
	if t == nil {
		return ""
	}
	return t.id
}

// newDeploymentID mints an opaque, unique id for one deployment operation.
// Phelix has no pre-existing per-operation identifier (the deploy lock records
// operation/pid/time, which is not stable across the operation's events), so
// one is generated here and reused for every event of the deployment.
func newDeploymentID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "dep-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return "dep-" + hex.EncodeToString(b)
}

// --- observation inputs ----------------------------------------------------

// Bind attaches the live DeployState the deployment mutates. The tracker reads
// it when building a snapshot, so slot/replica state is always whatever the
// flow has actually persisted — there is no second copy to drift. It also
// stamps the deployment id into the state, so a snapshot rebuilt later (by the
// monitor daemon after a reconnect) correlates with this deployment's events.
func (t *Tracker) Bind(state *DeployState) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.state = state
	if state != nil {
		state.LastDeploymentID = t.id
	}
	t.mu.Unlock()
}

// SetVersions records the version currently serving and the one this
// deployment targets. Both are "vN" labels; 0 means unknown and is reported as
// an empty string. A 0 target leaves any target already recorded untouched, so
// re-stating the current version mid-deployment cannot erase it.
func (t *Tracker) SetVersions(current, target int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if cur := versionLabel(current); cur != "" {
		t.currentVersion = cur
	}
	if tgt := versionLabel(target); tgt != "" {
		t.targetVersion = tgt
	}
	t.mu.Unlock()
}

// SetTargetVersion records only the target version (known after the build).
func (t *Tracker) SetTargetVersion(target int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.targetVersion = versionLabel(target)
	t.mu.Unlock()
}

// PromoteCurrentVersion records that the target version now serves traffic.
// Deploy flows call it only after the proxy switch succeeded, so a failed
// deployment never reports current_version == target_version.
func (t *Tracker) PromoteCurrentVersion() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.targetVersion != "" {
		t.currentVersion = t.targetVersion
	}
	t.mu.Unlock()
}

// SetReplicasDesired records the replica count the rollout was asked for. It
// is never used to derive observed counts.
func (t *Tracker) SetReplicasDesired(n int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.replicasDesired = n
	t.mu.Unlock()
}

// SetHealthConfig records the health configuration and the tier the deployment
// selected, from the app's persisted health config.
func (t *Tracker) SetHealthConfig(cfg *health.DeployTierConfig, tier health.Tier) {
	if t == nil {
		return
	}
	h := &HealthState{Tier: int(tier), TierLabel: tier.String()}
	if cfg != nil {
		h.Mode = string(cfg.Mode)
		h.Path = cfg.Path
		h.Retries = cfg.Retries
		h.Interval = cfg.Interval
		h.Timeout = cfg.Timeout
	}
	t.mu.Lock()
	if t.health != nil {
		h.LastHealthyAt = t.health.LastHealthyAt
	}
	t.health = h
	t.mu.Unlock()
}

// SetProxy records the proxy routing this deployment established: the label and
// internal port the proxy primary now points at, plus every upstream in the
// backend set. Called only after a successful Add/Switch, so it reflects the
// daemon's real state rather than an assumption.
func (t *Tracker) SetProxy(publicPort int, targetLabel string, targetPort int, upstreams []string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.proxy = &ProxyState{
		Enabled:            true,
		PublicPort:         publicPort,
		TargetLabel:        targetLabel,
		TargetInternalPort: targetPort,
		Upstreams:          append([]string(nil), upstreams...),
	}
	t.mu.Unlock()
}

// SetUnproxied records that this deployment serves its public port directly,
// with no proxy in front of it. That is the classic strategy's actual topology,
// and stating it explicitly is what lets the backend distinguish "not proxied"
// from "proxy state unknown".
func (t *Tracker) SetUnproxied(publicPort int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.proxy = &ProxyState{PublicPort: publicPort}
	t.mu.Unlock()
}

// MarkHealthy records that the instance identified by key (slot name for
// blue-green, replica index for rolling) passed its health window during this
// deployment. Only instances recorded here are ever reported as healthy.
func (t *Tracker) MarkHealthy(key string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.mu.Lock()
	t.healthy[key] = now
	if t.health == nil {
		t.health = &HealthState{}
	}
	t.health.LastHealthyAt = now
	t.mu.Unlock()
}

// --- emission --------------------------------------------------------------

// eventOpts carries the per-instance detail of one event.
type eventOpts struct {
	slot         string
	replicaID    string
	replicaIndex int
	internalPort int
	pid          int
	message      string
	failure      *Failure
}

// Started records the beginning of the deployment. current/target are version
// numbers (0 = unknown).
func (t *Tracker) Started(current, target int, message string) {
	if t == nil {
		return
	}
	t.SetVersions(current, target)
	t.transition(PhaseIdle, StatusInProgress)
	t.emit(EventStarted, eventOpts{message: message})
}

// Building marks the artifact-preparation phase (fresh compile or rollback
// target resolution).
func (t *Tracker) Building(message string) {
	if t == nil {
		return
	}
	t.transition(PhaseBuilding, StatusInProgress)
	t.emit(EventBuilding, eventOpts{message: message})
}

// InstanceStarted reports a blue-green candidate that has been spawned on an
// internal port. It is not healthy yet — the deployment phase says starting.
func (t *Tracker) InstanceStarted(slot string, pid, port int) {
	if t == nil {
		return
	}
	t.transition(PhaseStarting, StatusInProgress)
	t.emit(EventInstanceStarted, eventOpts{slot: slot, pid: pid, internalPort: port})
}

// HealthCheckStarted reports that the candidate's health window has begun.
func (t *Tracker) HealthCheckStarted(slot string, port int) {
	if t == nil {
		return
	}
	t.transition(PhaseHealthCheck, StatusInProgress)
	t.emit(EventHealthCheckStarted, eventOpts{slot: slot, internalPort: port})
}

// InstanceHealthy reports that the candidate passed its health window.
func (t *Tracker) InstanceHealthy(slot string, pid, port int) {
	if t == nil {
		return
	}
	t.MarkHealthy(slot)
	t.emit(EventInstanceHealthy, eventOpts{slot: slot, pid: pid, internalPort: port})
}

// ProxySwitching reports that traffic is about to be re-pointed (or the app
// enrolled for the first time). Emitted before the proxy call so a failure
// between switching and switched is visible.
func (t *Tracker) ProxySwitching(slot string, port int, message string) {
	if t == nil {
		return
	}
	t.transition(PhaseSwitching, StatusInProgress)
	t.emit(EventProxySwitching, eventOpts{slot: slot, internalPort: port, message: message})
}

// ProxySwitched reports that the proxy now routes to slot. Emitted only after
// the daemon confirmed the enrol/switch.
func (t *Tracker) ProxySwitched(slot string, port int, message string) {
	if t == nil {
		return
	}
	t.transition(PhasePromoting, StatusInProgress)
	t.emit(EventProxySwitched, eventOpts{slot: slot, internalPort: port, message: message})
}

// InstanceDraining reports that the previous instance has left the routing set
// and is being asked to finish in-flight requests.
func (t *Tracker) InstanceDraining(slot string, pid int) {
	if t == nil {
		return
	}
	t.transition(PhaseDraining, StatusInProgress)
	t.emit(EventInstanceDraining, eventOpts{slot: slot, pid: pid})
}

// InstanceStopped reports that the previous instance exited.
func (t *Tracker) InstanceStopped(slot string, pid int, message string) {
	if t == nil {
		return
	}
	t.emit(EventInstanceStopped, eventOpts{slot: slot, pid: pid, message: message})
}

// ReplicaStarted reports a rolling replacement spawned alongside the replica it
// will replace (which keeps serving until the membership swap).
func (t *Tracker) ReplicaStarted(index int, pid, port int) {
	if t == nil {
		return
	}
	t.transition(PhaseStarting, StatusInProgress)
	t.emit(EventReplicaStarted, replicaOpts(index, eventOpts{pid: pid, internalPort: port}))
}

// ReplicaHealthCheckStarted reports the replacement's health window beginning.
func (t *Tracker) ReplicaHealthCheckStarted(index int, port int) {
	if t == nil {
		return
	}
	t.transition(PhaseHealthCheck, StatusInProgress)
	t.emit(EventReplicaHealthCheckStarted, replicaOpts(index, eventOpts{internalPort: port}))
}

// ReplicaHealthy reports that the replacement passed its health window. It is
// not in rotation yet — that is ReplicaReplaced.
func (t *Tracker) ReplicaHealthy(index int, pid, port int) {
	if t == nil {
		return
	}
	t.MarkHealthy(strconv.Itoa(index))
	t.emit(EventReplicaHealthy, replicaOpts(index, eventOpts{pid: pid, internalPort: port}))
}

// ReplicaProxySwitching reports that the proxy's backend set is about to change
// so it admits this replica and drops the instance it replaces.
func (t *Tracker) ReplicaProxySwitching(index, port int, message string) {
	if t == nil {
		return
	}
	t.transition(PhaseSwitching, StatusInProgress)
	t.emit(EventProxySwitching, replicaOpts(index, eventOpts{internalPort: port, message: message}))
}

// ReplicaReplaced reports that the proxy membership now includes the
// replacement and excludes the old instance.
func (t *Tracker) ReplicaReplaced(index int, pid, port int, message string) {
	if t == nil {
		return
	}
	t.transition(PhasePromoting, StatusInProgress)
	t.emit(EventReplicaReplaced, replicaOpts(index, eventOpts{pid: pid, internalPort: port, message: message}))
}

// ReplicaDraining reports the old replica leaving rotation.
func (t *Tracker) ReplicaDraining(index int, pid int) {
	if t == nil {
		return
	}
	t.transition(PhaseDraining, StatusInProgress)
	t.emit(EventReplicaDraining, replicaOpts(index, eventOpts{pid: pid}))
}

// ReplicaStopped reports the old replica's exit.
func (t *Tracker) ReplicaStopped(index int, pid int, message string) {
	if t == nil {
		return
	}
	t.emit(EventReplicaStopped, replicaOpts(index, eventOpts{pid: pid, message: message}))
}

// Completed marks the deployment successful. Callers invoke it only once the
// new version actually serves traffic.
func (t *Tracker) Completed(message string) {
	if t == nil {
		return
	}
	t.transition(PhaseCompleted, StatusSucceeded)
	t.emit(EventCompleted, eventOpts{message: message})
}

// Failed marks the deployment failed and records the structured failure. The
// current version is left untouched, so a failure before promotion reports the
// old version as still current.
func (t *Tracker) Failed(err error) {
	if t == nil || err == nil {
		return
	}
	f := failureFromError(err)
	t.mu.Lock()
	t.failure = f
	t.mu.Unlock()
	t.transition(PhaseFailed, StatusFailed)
	t.emit(EventFailed, eventOpts{failure: f, message: f.Message})
}

// Cancelled marks a deployment stopped by context cancellation.
func (t *Tracker) Cancelled(err error) {
	if t == nil {
		return
	}
	var f *Failure
	if err != nil {
		f = failureFromError(err)
	}
	t.mu.Lock()
	t.failure = f
	t.mu.Unlock()
	t.transition(PhaseCancelled, StatusCancelled)
	opts := eventOpts{failure: f}
	if f != nil {
		opts.message = f.Message
	}
	t.emit(EventCancelled, opts)
}

func replicaOpts(index int, o eventOpts) eventOpts {
	o.replicaID = replicaID(index)
	o.replicaIndex = index
	return o
}

// replicaID is the stable identity of a rolling replica. Phelix already keys
// replicas by index in deploy.json; the label mirrors the proxy target label
// ("replica-N") so backend correlation between telemetry and proxy upstreams
// needs no translation.
func replicaID(index int) string { return "replica-" + strconv.Itoa(index) }

func (t *Tracker) transition(phase, status string) {
	t.mu.Lock()
	t.phase = phase
	t.status = status
	t.mu.Unlock()
}

func (t *Tracker) emit(name string, o eventOpts) {
	t.mu.Lock()
	ev := Event{
		AppID:           t.appID,
		AppName:         t.appName,
		DeploymentID:    t.id,
		Event:           name,
		Strategy:        t.strategy,
		Phase:           t.phase,
		Status:          t.status,
		CurrentVersion:  t.currentVersion,
		TargetVersion:   t.targetVersion,
		Slot:            o.slot,
		ReplicaID:       o.replicaID,
		ReplicaIndex:    o.replicaIndex,
		InternalPort:    o.internalPort,
		PID:             o.pid,
		ReplicasDesired: t.replicasDesired,
		Message:         o.message,
		Failure:         o.failure,
		Timestamp:       time.Now(),
	}
	snap := t.snapshotLocked()
	sink := t.sink
	t.mu.Unlock()

	ev.Snapshot = snap
	if sink != nil {
		sink.Deployment(ev)
	}
}

// Snapshot returns the current deployment topology.
func (t *Tracker) Snapshot() *Snapshot {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}

func (t *Tracker) snapshotLocked() *Snapshot {
	s := &Snapshot{
		AppID:           t.appID,
		AppName:         t.appName,
		DeploymentID:    t.id,
		Strategy:        t.strategy,
		Phase:           t.phase,
		Status:          t.status,
		CurrentVersion:  t.currentVersion,
		TargetVersion:   t.targetVersion,
		ReplicasDesired: t.replicasDesired,
		Failure:         t.failure,
		StartedAt:       t.startedAt,
		UpdatedAt:       time.Now(),
	}
	if t.proxy != nil {
		cp := *t.proxy
		cp.Upstreams = append([]string(nil), t.proxy.Upstreams...)
		s.Proxy = &cp
	}
	if t.health != nil {
		cp := *t.health
		s.Health = &cp
	}
	if t.state == nil {
		return s
	}

	s.ActiveSlot = t.state.ActiveSlot
	for _, name := range []string{SlotBlue, SlotGreen} {
		inst := t.state.Slots[name]
		if inst == nil {
			continue
		}
		s.Slots = append(s.Slots, SlotState{
			Slot:         name,
			Version:      versionLabel(inst.Version),
			Status:       instanceStatus(inst),
			Health:       t.instanceHealthLocked(name, inst),
			InternalPort: inst.Port,
			PID:          inst.PID,
			Active:       name == t.state.ActiveSlot,
			StartedAt:    inst.StartedAt,
			HealthyAt:    t.healthy[name],
		})
	}

	for _, key := range replicaIndices(t.state.Replicas) {
		inst := t.state.Replicas[key]
		if inst == nil {
			continue
		}
		idx, err := strconv.Atoi(key)
		if err != nil {
			continue
		}
		status := instanceStatus(inst)
		health := t.instanceHealthLocked(key, inst)
		s.Replicas = append(s.Replicas, ReplicaState{
			ID:           replicaID(idx),
			Index:        idx,
			Version:      versionLabel(inst.Version),
			Status:       status,
			Health:       health,
			InternalPort: inst.Port,
			PID:          inst.PID,
			StartedAt:    inst.StartedAt,
			HealthyAt:    t.healthy[key],
		})
		// Observed counters only. A replica counts as current when a process is
		// recorded for it, ready when that process is in rotation, and healthy
		// only when this deployment saw it pass its health window.
		if inst.PID > 0 {
			s.ReplicasCurrent++
		}
		if inst.PID > 0 && status == "running" {
			s.ReplicasReady++
			if health == HealthHealthy {
				s.ReplicasHealthy++
			}
		}
	}
	return s
}

// instanceHealthLocked reports what Phelix actually knows about one instance's
// health: healthy only when it passed its window during this deployment,
// unhealthy when the flow recorded a failure for it, pending while it is
// starting or being replaced, and unknown otherwise.
func (t *Tracker) instanceHealthLocked(key string, inst *Instance) string {
	if inst == nil {
		return HealthUnknown
	}
	switch inst.Status {
	case "failed":
		return HealthUnhealthy
	case "starting", "replacing":
		return HealthPending
	}
	if _, ok := t.healthy[key]; ok && inst.PID > 0 {
		return HealthHealthy
	}
	// A running instance from an earlier deployment: this deployment has no
	// health evidence for it, and inventing one would be a fabrication.
	return HealthUnknown
}

// instanceStatus normalises the persisted status, defaulting to "stopped" for
// records that never carried one.
func instanceStatus(inst *Instance) string {
	if inst == nil || inst.Status == "" {
		return "stopped"
	}
	return inst.Status
}

func versionLabel(v int) string {
	if v <= 0 {
		return ""
	}
	return "v" + strconv.Itoa(v)
}

// retryableCodes are the failure categories where repeating the same
// deployment could plausibly succeed without a code or configuration change.
var retryableCodes = map[phelixerr.Code]bool{
	phelixerr.CodeHealthCheckFailed:   true,
	phelixerr.CodeInstanceStartFailed: true,
	phelixerr.CodeProxy:               true,
	phelixerr.CodeConnection:          true,
	phelixerr.CodeTimeout:             true,
	phelixerr.CodePortUnavailable:     true,
	phelixerr.CodeDeployLocked:        true,
	phelixerr.CodeNetwork:             true,
	phelixerr.CodeProcessFailed:       true,
}

// failureFromError maps a deployment error onto the wire failure shape using
// the existing structured error model. The message is redacted so a cause that
// embedded a credential cannot reach the backend.
func failureFromError(err error) *Failure {
	code := phelixerr.CodeOf(err)
	return &Failure{
		Code:      code.String(),
		Message:   phelixerr.Redact(err.Error()),
		Retryable: retryableCodes[code],
	}
}

// --- flow helpers ----------------------------------------------------------

// reportDeployFailure records a terminal deployment error, distinguishing a
// cancellation (the operator or a deadline stopped it) from a genuine failure.
func reportDeployFailure(t *Tracker, err error) {
	if t == nil || err == nil {
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Cancelled(err)
		return
	}
	t.Failed(err)
}

// activeVersionOf resolves the version that is serving traffic right now:
// deploy.json's ActiveVersion when the app has one, else the version marked
// current in versions.json (classic apps, or a first zero-downtime deploy).
// Returns 0 when neither is known.
func activeVersionOf(state *DeployState, appName string) int {
	if state != nil && state.ActiveVersion > 0 {
		return state.ActiveVersion
	}
	cur, err := CurrentVersion(appName)
	if err != nil {
		return 0
	}
	return cur
}

// drainOutcome describes how an old instance left: it is the same distinction
// the deploy log makes, so telemetry and log agree on whether a SIGKILL was
// needed.
func drainOutcome(report ShutdownReport) string {
	if report.ForceKilled {
		return "force-killed after grace period"
	}
	return "drained and exited"
}

// upstreamHosts renders proxy targets as host:port strings for telemetry.
func upstreamHosts(primary proxy.Target, backends []proxy.Target) []string {
	out := make([]string, 0, len(backends)+1)
	if primary.Host != "" {
		out = append(out, primary.Host)
	}
	for _, b := range backends {
		if b.Host != "" {
			out = append(out, b.Host)
		}
	}
	return out
}
