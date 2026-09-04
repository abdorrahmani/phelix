package deploy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
)

// recordingSink captures every telemetry event so tests can assert on the
// sequence, the correlation id, and the snapshot attached to each event.
type recordingSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *recordingSink) Deployment(ev Event) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
}

func (s *recordingSink) all() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

func (s *recordingSink) names() []string {
	out := []string{}
	for _, ev := range s.all() {
		out = append(out, ev.Event)
	}
	return out
}

// find returns the first event with the given name.
func (s *recordingSink) find(name string) (Event, bool) {
	for _, ev := range s.all() {
		if ev.Event == name {
			return ev, true
		}
	}
	return Event{}, false
}

// last returns the final event emitted.
func (s *recordingSink) last(t *testing.T) Event {
	t.Helper()
	all := s.all()
	if len(all) == 0 {
		t.Fatalf("no telemetry events emitted")
	}
	return all[len(all)-1]
}

// indexOf returns the position of the first event with the given name, or -1.
func indexOf(names []string, want string) int {
	for i, n := range names {
		if n == want {
			return i
		}
	}
	return -1
}

func requireOrder(t *testing.T, names []string, sequence ...string) {
	t.Helper()
	prev := -1
	for _, want := range sequence {
		at := indexOf(names, want)
		if at < 0 {
			t.Fatalf("missing event %q in %v", want, names)
		}
		if at < prev {
			t.Fatalf("event %q out of order in %v", want, names)
		}
		prev = at
	}
}

// --- blue-green ------------------------------------------------------------

func TestTelemetry_BlueGreen_FirstDeploy_EnrolAndComplete(t *testing.T) {
	resetHome(t)

	sink := &recordingSink{}
	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	bg := &BlueGreen{
		AppName: "tapp", AppID: "42", PublicPort: 3000,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    100 * time.Millisecond,
		Telemetry:      NewTracker(sink, "42", "tapp", string(ModeBlueGreen)),
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	names := sink.names()
	requireOrder(t, names,
		EventStarted, EventBuilding, EventInstanceStarted,
		EventHealthCheckStarted, EventInstanceHealthy,
		EventProxySwitching, EventProxySwitched, EventCompleted,
	)

	// One deployment id across every event.
	id := bg.Telemetry.DeploymentID()
	if id == "" {
		t.Fatalf("expected a deployment id")
	}
	for _, ev := range sink.all() {
		if ev.DeploymentID != id {
			t.Fatalf("event %s has deployment id %q, want %q", ev.Event, ev.DeploymentID, id)
		}
		if ev.Strategy != string(ModeBlueGreen) {
			t.Fatalf("event %s strategy = %q, want blue-green", ev.Event, ev.Strategy)
		}
		if ev.AppID != "42" || ev.AppName != "tapp" {
			t.Fatalf("event %s identity = %q/%q", ev.Event, ev.AppID, ev.AppName)
		}
		if ev.Snapshot == nil {
			t.Fatalf("event %s carries no snapshot", ev.Event)
		}
	}

	// Terminal event: completed, and the active slot serves.
	done := sink.last(t)
	if done.Event != EventCompleted || done.Status != StatusSucceeded || done.Phase != PhaseCompleted {
		t.Fatalf("terminal event = %s/%s/%s", done.Event, done.Status, done.Phase)
	}
	state := mustLoad(t, "tapp")
	snap := done.Snapshot
	if snap.ActiveSlot != state.ActiveSlot {
		t.Fatalf("snapshot active slot %q != state %q", snap.ActiveSlot, state.ActiveSlot)
	}
	if len(snap.Slots) != 2 {
		t.Fatalf("expected both slots reported, got %d", len(snap.Slots))
	}
	var active *SlotState
	for i := range snap.Slots {
		if snap.Slots[i].Active {
			active = &snap.Slots[i]
		}
	}
	if active == nil {
		t.Fatalf("no slot marked active in %+v", snap.Slots)
	}
	if active.Health != HealthHealthy {
		t.Fatalf("active slot health = %q, want healthy", active.Health)
	}
	if active.InternalPort == 0 || active.PID == 0 {
		t.Fatalf("active slot missing port/pid: %+v", active)
	}
	// Proxy state comes from the enrol that actually happened.
	if snap.Proxy == nil || !snap.Proxy.Enabled {
		t.Fatalf("expected proxy state, got %+v", snap.Proxy)
	}
	if snap.Proxy.PublicPort != 3000 {
		t.Fatalf("proxy public port = %d, want 3000", snap.Proxy.PublicPort)
	}
	if snap.Proxy.TargetLabel != active.Slot {
		t.Fatalf("proxy target %q != active slot %q", snap.Proxy.TargetLabel, active.Slot)
	}
	if snap.Proxy.TargetInternalPort != active.InternalPort {
		t.Fatalf("proxy target port %d != active slot port %d", snap.Proxy.TargetInternalPort, active.InternalPort)
	}
	// Health config the deployment actually selected.
	if snap.Health == nil || snap.Health.Tier == 0 {
		t.Fatalf("expected health tier in snapshot, got %+v", snap.Health)
	}
}

func TestTelemetry_BlueGreen_SecondDeploy_SwitchDrainAndPromote(t *testing.T) {
	resetHome(t)

	sink := &recordingSink{}
	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	newBG := func(tr *Tracker) *BlueGreen {
		return &BlueGreen{
			AppName: "sapp", AppID: "7", PublicPort: 3100,
			Builder:        stubBuilder("/bin/true"),
			Launcher:       fl.Launch,
			ProxyClient:    pc,
			Logger:         &fakeLogger{},
			HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
			GracePeriod:    100 * time.Millisecond,
			Telemetry:      tr,
		}
	}

	// First deploy (untracked) establishes an active slot.
	if err := newBG(nil).Deploy(context.Background()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	first := mustLoad(t, "sapp")
	firstActive := first.ActiveSlot
	// Point the old instance at a PID that is not us, so the drain step is a
	// no-op instead of signalling the test process.
	first.Slots[firstActive].PID = 999999
	if err := Store(first); err != nil {
		t.Fatalf("store: %v", err)
	}

	tracker := NewTracker(sink, "7", "sapp", string(ModeBlueGreen))
	if err := newBG(tracker).Deploy(context.Background()); err != nil {
		t.Fatalf("second deploy: %v", err)
	}

	names := sink.names()
	requireOrder(t, names,
		EventInstanceHealthy, EventProxySwitching, EventProxySwitched,
		EventInstanceDraining, EventInstanceStopped, EventCompleted,
	)

	drain, ok := sink.find(EventInstanceDraining)
	if !ok {
		t.Fatalf("no draining event")
	}
	if drain.Slot != firstActive {
		t.Fatalf("draining slot = %q, want previous active %q", drain.Slot, firstActive)
	}
	if drain.Phase != PhaseDraining {
		t.Fatalf("draining phase = %q", drain.Phase)
	}

	after := mustLoad(t, "sapp")
	done := sink.last(t)
	if done.Snapshot.ActiveSlot != after.ActiveSlot {
		t.Fatalf("snapshot slot %q != state %q", done.Snapshot.ActiveSlot, after.ActiveSlot)
	}
	// The drained slot is reported stopped, not healthy.
	for _, sl := range done.Snapshot.Slots {
		if sl.Slot == firstActive {
			if sl.Status != "stopped" {
				t.Fatalf("old slot status = %q, want stopped", sl.Status)
			}
			if sl.Health == HealthHealthy {
				t.Fatalf("stopped slot must not report healthy")
			}
		}
	}
	// Deployment id is persisted so a later resync can correlate.
	if after.LastDeploymentID != tracker.DeploymentID() {
		t.Fatalf("state deployment id = %q, want %q", after.LastDeploymentID, tracker.DeploymentID())
	}
}

func TestTelemetry_BlueGreen_HealthFailure_KeepsOldVersionCurrent(t *testing.T) {
	resetHome(t)

	sink := &recordingSink{}
	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	bg := &BlueGreen{
		AppName: "fapp", AppID: "9", PublicPort: 3200,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    100 * time.Millisecond,
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	// Record a version so "current" is a real value the failure must preserve.
	seedVersion(t, "fapp", 14, true)
	state := mustLoad(t, "fapp")
	state.ActiveVersion = 14
	if err := Store(state); err != nil {
		t.Fatalf("store: %v", err)
	}

	// Second deploy: candidate never answers, so health fails before any switch.
	tracker := NewTracker(sink, "9", "fapp", string(ModeBlueGreen))
	bg.Telemetry = tracker
	bg.Launcher = closedLauncher{}.Launch
	bg.HealthProvider = func(string) *health.DeployTierConfig { return fastHealthShortTimeout() }
	if err := bg.Deploy(context.Background()); err == nil {
		t.Fatalf("expected health failure")
	}

	names := sink.names()
	if indexOf(names, EventProxySwitched) >= 0 {
		t.Fatalf("traffic must not be reported as switched on health failure: %v", names)
	}
	fail := sink.last(t)
	if fail.Event != EventFailed || fail.Status != StatusFailed || fail.Phase != PhaseFailed {
		t.Fatalf("terminal event = %s/%s/%s", fail.Event, fail.Status, fail.Phase)
	}
	if fail.Failure == nil {
		t.Fatalf("failed event carries no failure detail")
	}
	if fail.Failure.Code != string(phelixerr.CodeHealthCheckFailed) {
		t.Fatalf("failure code = %q, want %s", fail.Failure.Code, phelixerr.CodeHealthCheckFailed)
	}
	if !fail.Failure.Retryable {
		t.Fatalf("health failure should be retryable")
	}
	if fail.Failure.Message == "" {
		t.Fatalf("failure message is empty")
	}
	// The old version stays current: no promotion happened.
	if fail.CurrentVersion != "v14" {
		t.Fatalf("current version = %q, want v14 (unchanged)", fail.CurrentVersion)
	}
	if fail.CurrentVersion == fail.TargetVersion && fail.TargetVersion != "" {
		t.Fatalf("current must not equal target on failure: %q", fail.CurrentVersion)
	}
	// The failed slot reports unhealthy, the serving one is untouched.
	snap := fail.Snapshot
	if snap == nil {
		t.Fatalf("no snapshot on failure event")
	}
	sawUnhealthy := false
	for _, sl := range snap.Slots {
		if sl.Status == "failed" {
			sawUnhealthy = sl.Health == HealthUnhealthy
		}
	}
	if !sawUnhealthy {
		t.Fatalf("expected the failed slot to report unhealthy: %+v", snap.Slots)
	}
}

func TestTelemetry_BlueGreen_ProxySwitchFailure_ReportsProxyCode(t *testing.T) {
	resetHome(t)

	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	bg := &BlueGreen{
		AppName: "papp", AppID: "11", PublicPort: 3300,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    100 * time.Millisecond,
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	st := mustLoad(t, "papp")
	st.Slots[st.ActiveSlot].PID = 999999
	if err := Store(st); err != nil {
		t.Fatalf("store: %v", err)
	}

	sink := &recordingSink{}
	bg.Telemetry = NewTracker(sink, "11", "papp", string(ModeBlueGreen))
	bg.ProxyClient = &failSwitchClient{inner: pc, err: errors.New("daemon refused")}
	if err := bg.Deploy(context.Background()); err == nil {
		t.Fatalf("expected switch failure")
	}

	names := sink.names()
	if indexOf(names, EventProxySwitching) < 0 {
		t.Fatalf("expected a switching event before the failure: %v", names)
	}
	if indexOf(names, EventProxySwitched) >= 0 {
		t.Fatalf("switched must not be reported when the switch failed: %v", names)
	}
	fail := sink.last(t)
	if fail.Failure == nil || fail.Failure.Code != string(phelixerr.CodeProxy) {
		t.Fatalf("failure = %+v, want PROXY_ERROR", fail.Failure)
	}
}

// --- rolling ---------------------------------------------------------------

func TestTelemetry_Rolling_SingleReplica_CountsAreObserved(t *testing.T) {
	resetHome(t)

	sink := &recordingSink{}
	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	r := &Rolling{
		AppName: "rapp", AppID: "21", PublicPort: 3400, Replicas: 1,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    100 * time.Millisecond,
		Telemetry:      NewTracker(sink, "21", "rapp", string(ModeRolling)),
	}
	if err := r.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	names := sink.names()
	requireOrder(t, names,
		EventStarted, EventBuilding, EventReplicaStarted,
		EventReplicaHealthCheckStarted, EventReplicaHealthy,
		EventProxySwitching, EventReplicaReplaced, EventCompleted,
	)

	done := sink.last(t)
	snap := done.Snapshot
	if snap.ReplicasDesired != 1 {
		t.Fatalf("desired = %d, want 1", snap.ReplicasDesired)
	}
	if snap.ReplicasCurrent != 1 || snap.ReplicasReady != 1 || snap.ReplicasHealthy != 1 {
		t.Fatalf("counts current/ready/healthy = %d/%d/%d, want 1/1/1",
			snap.ReplicasCurrent, snap.ReplicasReady, snap.ReplicasHealthy)
	}
	if len(snap.Replicas) != 1 {
		t.Fatalf("expected one replica, got %d", len(snap.Replicas))
	}
	rep := snap.Replicas[0]
	if rep.ID != "replica-0" || rep.Index != 0 {
		t.Fatalf("replica identity = %q/%d", rep.ID, rep.Index)
	}
	if rep.Health != HealthHealthy || rep.Status != "running" {
		t.Fatalf("replica health/status = %q/%q", rep.Health, rep.Status)
	}
	if rep.InternalPort == 0 {
		t.Fatalf("replica has no internal port")
	}
	// Proxy membership reflects the real upstream set.
	if snap.Proxy == nil || len(snap.Proxy.Upstreams) != 1 {
		t.Fatalf("proxy upstreams = %+v", snap.Proxy)
	}
	if !strings.Contains(snap.Proxy.Upstreams[0], "127.0.0.1:") {
		t.Fatalf("upstream %q is not a host:port", snap.Proxy.Upstreams[0])
	}
}

func TestTelemetry_Rolling_MultipleReplicas_PerReplicaEvents(t *testing.T) {
	resetHome(t)

	sink := &recordingSink{}
	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	r := &Rolling{
		AppName: "mapp", AppID: "22", PublicPort: 3500, Replicas: 3,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    100 * time.Millisecond,
		Telemetry:      NewTracker(sink, "22", "mapp", string(ModeRolling)),
	}
	if err := r.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// Every replica index is reported, and its own events keep index order.
	seen := map[int][]string{}
	for _, ev := range sink.all() {
		if ev.ReplicaID == "" {
			continue
		}
		seen[ev.ReplicaIndex] = append(seen[ev.ReplicaIndex], ev.Event)
		if ev.ReplicaID != replicaID(ev.ReplicaIndex) {
			t.Fatalf("replica id %q does not match index %d", ev.ReplicaID, ev.ReplicaIndex)
		}
		if ev.Slot != "" {
			t.Fatalf("replica event %s must not set slot (got %q)", ev.Event, ev.Slot)
		}
	}
	for i := 0; i < 3; i++ {
		evs, ok := seen[i]
		if !ok {
			t.Fatalf("replica %d produced no events (saw %v)", i, seen)
		}
		requireOrder(t, evs, EventReplicaStarted, EventReplicaHealthCheckStarted,
			EventReplicaHealthy, EventReplicaReplaced)
	}

	done := sink.last(t)
	if done.Snapshot.ReplicasDesired != 3 {
		t.Fatalf("desired = %d, want 3", done.Snapshot.ReplicasDesired)
	}
	if done.Snapshot.ReplicasHealthy != 3 {
		t.Fatalf("healthy = %d, want 3", done.Snapshot.ReplicasHealthy)
	}
}

func TestTelemetry_Rolling_HealthFailure_DoesNotCountDesiredAsHealthy(t *testing.T) {
	resetHome(t)

	sink := &recordingSink{}
	pc := &fakeProxyClient{alive: true}

	r := &Rolling{
		AppName: "xapp", AppID: "23", PublicPort: 3600, Replicas: 3,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       closedLauncher{}.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealthShortTimeout() },
		GracePeriod:    100 * time.Millisecond,
		Telemetry:      NewTracker(sink, "23", "xapp", string(ModeRolling)),
	}
	if err := r.Deploy(context.Background()); err == nil {
		t.Fatalf("expected rollout failure")
	}

	fail := sink.last(t)
	if fail.Event != EventFailed {
		t.Fatalf("terminal event = %s, want failed", fail.Event)
	}
	snap := fail.Snapshot
	if snap.ReplicasDesired != 3 {
		t.Fatalf("desired = %d, want 3", snap.ReplicasDesired)
	}
	if snap.ReplicasHealthy != 0 {
		t.Fatalf("healthy = %d; desired replicas must never be reported as healthy", snap.ReplicasHealthy)
	}
	if indexOf(sink.names(), EventReplicaHealthy) >= 0 {
		t.Fatalf("no replica became healthy, so no healthy event may be emitted")
	}
}

func TestTelemetry_Rolling_Cancelled(t *testing.T) {
	resetHome(t)

	sink := &recordingSink{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := &Rolling{
		AppName: "capp", AppID: "24", PublicPort: 3700, Replicas: 1,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       closedLauncher{}.Launch,
		ProxyClient:    &fakeProxyClient{alive: true},
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealthShortTimeout() },
		Telemetry:      NewTracker(sink, "24", "capp", string(ModeRolling)),
	}
	if err := r.Deploy(ctx); err == nil {
		t.Fatalf("expected cancellation error")
	}

	last := sink.last(t)
	if last.Event != EventCancelled || last.Status != StatusCancelled {
		t.Fatalf("terminal event = %s/%s, want cancelled", last.Event, last.Status)
	}
}

// --- classic ---------------------------------------------------------------

// The classic path is instrumented by the CLI (cmd/deploy_telemetry.go), which
// drives the same Tracker. This exercises that contract without a CLI.
func TestTelemetry_Classic_StartCompleteAndFail(t *testing.T) {
	sink := &recordingSink{}
	tr := NewTracker(sink, "1", "capp", StrategyClassic)
	tr.SetUnproxied(8080)
	tr.Started(3, 0, "classic deploy")
	tr.Building("classic build")
	tr.SetTargetVersion(4)
	tr.InstanceStarted("", 4242, 8080)
	tr.PromoteCurrentVersion()
	tr.Completed("running")

	requireOrder(t, sink.names(), EventStarted, EventBuilding, EventInstanceStarted, EventCompleted)
	done := sink.last(t)
	if done.Strategy != StrategyClassic {
		t.Fatalf("strategy = %q, want classic", done.Strategy)
	}
	if done.CurrentVersion != "v4" || done.TargetVersion != "v4" {
		t.Fatalf("versions after promotion = %q/%q, want v4/v4", done.CurrentVersion, done.TargetVersion)
	}
	// Classic serves its port directly: proxy is present but not enabled.
	if done.Snapshot.Proxy == nil {
		t.Fatalf("classic snapshot should state proxy state explicitly")
	}
	if done.Snapshot.Proxy.Enabled {
		t.Fatalf("classic deploy must not report an enabled proxy")
	}
	if done.Snapshot.Proxy.PublicPort != 8080 {
		t.Fatalf("public port = %d, want 8080", done.Snapshot.Proxy.PublicPort)
	}

	// A build failure keeps the old version current.
	fsink := &recordingSink{}
	ft := NewTracker(fsink, "1", "capp", StrategyClassic)
	ft.Started(3, 0, "classic deploy")
	ft.Building("classic build")
	ft.Failed(phelixerr.New(phelixerr.CodeBuildFailed, "compile error"))

	fail := fsink.last(t)
	if fail.Failure == nil || fail.Failure.Code != string(phelixerr.CodeBuildFailed) {
		t.Fatalf("failure = %+v, want BUILD_FAILED", fail.Failure)
	}
	if fail.Failure.Retryable {
		t.Fatalf("a build failure is not retryable without a code change")
	}
	if fail.CurrentVersion != "v3" {
		t.Fatalf("current version = %q, want v3", fail.CurrentVersion)
	}
}

// --- nil tracker (telemetry disabled) --------------------------------------

func TestTelemetry_NilTracker_IsSilentAndSafe(t *testing.T) {
	if got := NewTracker(nil, "1", "app", StrategyClassic); got != nil {
		t.Fatalf("NewTracker with no sink should return nil, got %+v", got)
	}
	var tr *Tracker // exactly what a deploy gets when telemetry is off
	tr.Bind(&DeployState{})
	tr.Started(1, 2, "x")
	tr.Building("x")
	tr.SetVersions(1, 2)
	tr.SetTargetVersion(2)
	tr.SetReplicasDesired(3)
	tr.SetHealthConfig(nil, health.Tier1HTTPPath)
	tr.SetProxy(1, "blue", 2, nil)
	tr.SetUnproxied(1)
	tr.MarkHealthy("blue")
	tr.InstanceStarted("blue", 1, 2)
	tr.HealthCheckStarted("blue", 2)
	tr.InstanceHealthy("blue", 1, 2)
	tr.ProxySwitching("blue", 2, "")
	tr.ProxySwitched("blue", 2, "")
	tr.InstanceDraining("green", 3)
	tr.InstanceStopped("green", 3, "")
	tr.ReplicaStarted(0, 1, 2)
	tr.ReplicaHealthCheckStarted(0, 2)
	tr.ReplicaHealthy(0, 1, 2)
	tr.ReplicaReplaced(0, 1, 2, "")
	tr.ReplicaDraining(0, 1)
	tr.ReplicaStopped(0, 1, "")
	tr.PromoteCurrentVersion()
	tr.Completed("")
	tr.Failed(errors.New("x"))
	tr.Cancelled(context.Canceled)
	if tr.DeploymentID() != "" {
		t.Fatalf("nil tracker must have no deployment id")
	}
	if tr.Snapshot() != nil {
		t.Fatalf("nil tracker must produce no snapshot")
	}
}

// --- helpers ---------------------------------------------------------------

// seedVersion writes a minimal versions.json row so version-sensitive
// assertions have a real "current" value to preserve.
func seedVersion(t *testing.T, appName string, ver int, current bool) {
	t.Helper()
	vf, err := LoadVersions(appName)
	if err != nil {
		t.Fatalf("load versions: %v", err)
	}
	vf.Versions = append(vf.Versions, VersionMeta{
		Version:   ver,
		BuiltAt:   time.Now(),
		IsCurrent: current,
	})
	if err := saveVersions(appName, vf); err != nil {
		t.Fatalf("save versions: %v", err)
	}
}
