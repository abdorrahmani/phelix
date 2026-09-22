package deploy

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/proxy"
)

// The resync path rebuilds a snapshot for a consumer that did not run the
// deployment (the monitor daemon after a reconnect). These tests pin the two
// properties the backend depends on: it must describe persisted reality, and it
// must never upgrade an unproven instance to "healthy".

// statusOnlyProxy answers Status from a canned table and rejects every mutation
// — the resync path must only ever read.
type statusOnlyProxy struct {
	statuses []proxy.AppStatus
	err      error
}

func (p *statusOnlyProxy) Ping(context.Context) error { return nil }
func (p *statusOnlyProxy) Add(context.Context, string, int, proxy.Target, ...proxy.Target) error {
	panic("resync must not mutate proxy state")
}
func (p *statusOnlyProxy) Switch(context.Context, string, proxy.Target, ...proxy.Target) error {
	panic("resync must not mutate proxy state")
}
func (p *statusOnlyProxy) Remove(context.Context, string) error {
	panic("resync must not mutate proxy state")
}
func (p *statusOnlyProxy) Status(_ context.Context, _ string) ([]proxy.AppStatus, error) {
	return p.statuses, p.err
}

func TestSnapshotForApp_NoDeployState(t *testing.T) {
	resetHome(t)
	if got := SnapshotForApp(context.Background(), "ghost", "1", nil); got != nil {
		t.Fatalf("app without deploy state should produce no snapshot, got %+v", got)
	}
}

func TestSnapshotForApp_BlueGreen_ReportsBothSlotsAndProxyTarget(t *testing.T) {
	resetHome(t)

	self := os.Getpid()
	state := &DeployState{
		AppName: "bgapp", AppID: "5", Mode: ModeBlueGreen, PublicPort: 3000,
		ActiveSlot:       SlotGreen,
		ActiveVersion:    15,
		LastDeploymentID: "dep-abc",
		LastRequestID:    "req-abc",
		Slots: map[string]*Instance{
			SlotBlue: {
				Slot: SlotBlue, Version: 14, Status: "stopped", Port: 49152, PID: 0,
			},
			SlotGreen: {
				Slot: SlotGreen, Version: 15, Status: "running", Port: 49153,
				PID: self, BinaryPath: mustSelfExe(t), StartedAt: time.Now(),
			},
		},
		Health: &HealthSummary{Tier: 1, TierLabel: "Tier 1", HealthyAt: time.Now()},
	}
	if err := Store(state); err != nil {
		t.Fatalf("store: %v", err)
	}

	pc := &statusOnlyProxy{statuses: []proxy.AppStatus{{
		AppName:    "bgapp",
		PublicPort: 3000,
		Primary:    proxy.Target{Host: "127.0.0.1:49153", Label: SlotGreen},
		InFlight:   2,
	}}}

	snap := SnapshotForApp(context.Background(), "bgapp", "5", pc)
	if snap == nil {
		t.Fatalf("expected a snapshot")
	}
	if snap.Strategy != string(ModeBlueGreen) {
		t.Fatalf("strategy = %q", snap.Strategy)
	}
	if snap.DeploymentID != "dep-abc" {
		t.Fatalf("deployment id = %q, want the persisted dep-abc", snap.DeploymentID)
	}
	if snap.RequestID != "req-abc" {
		t.Fatalf("request id = %q, want the persisted req-abc", snap.RequestID)
	}
	if snap.CurrentVersion != "v15" {
		t.Fatalf("current version = %q, want v15", snap.CurrentVersion)
	}
	if snap.ActiveSlot != SlotGreen {
		t.Fatalf("active slot = %q", snap.ActiveSlot)
	}
	if len(snap.Slots) != 2 {
		t.Fatalf("expected both slots, got %d", len(snap.Slots))
	}
	byName := map[string]SlotState{}
	for _, s := range snap.Slots {
		byName[s.Slot] = s
	}
	green := byName[SlotGreen]
	if !green.Active || green.Health != HealthHealthy || green.InternalPort != 49153 {
		t.Fatalf("green slot = %+v", green)
	}
	blue := byName[SlotBlue]
	if blue.Active {
		t.Fatalf("blue must not be active")
	}
	if blue.Health == HealthHealthy {
		t.Fatalf("a stopped slot must not report healthy: %+v", blue)
	}
	if blue.Version != "v14" {
		t.Fatalf("blue version = %q, want v14", blue.Version)
	}
	// Proxy state is what the daemon reported, not an inference.
	if snap.Proxy == nil || !snap.Proxy.Enabled {
		t.Fatalf("proxy = %+v", snap.Proxy)
	}
	if snap.Proxy.PublicPort != 3000 || snap.Proxy.TargetLabel != SlotGreen || snap.Proxy.TargetInternalPort != 49153 {
		t.Fatalf("proxy routing = %+v", snap.Proxy)
	}
	if snap.Proxy.InFlight != 2 {
		t.Fatalf("in-flight = %d, want 2", snap.Proxy.InFlight)
	}
	if snap.Phase != PhaseCompleted || snap.Status != StatusSucceeded {
		t.Fatalf("phase/status = %s/%s, want completed/succeeded", snap.Phase, snap.Status)
	}
}

func TestSnapshotForApp_DeadInstance_NotHealthyNotServing(t *testing.T) {
	resetHome(t)

	state := &DeployState{
		AppName: "deadapp", AppID: "6", Mode: ModeBlueGreen, PublicPort: 3001,
		ActiveSlot:    SlotBlue,
		ActiveVersion: 3,
		Slots: map[string]*Instance{
			// A PID that is not running: deploy.json claims running, reality says no.
			SlotBlue: {Slot: SlotBlue, Version: 3, Status: "running", Port: 40000, PID: 999999},
		},
		Health: &HealthSummary{Tier: 1, HealthyAt: time.Now()},
	}
	if err := Store(state); err != nil {
		t.Fatalf("store: %v", err)
	}

	snap := SnapshotForApp(context.Background(), "deadapp", "6", nil)
	if snap == nil {
		t.Fatalf("expected a snapshot")
	}
	if len(snap.Slots) != 1 {
		t.Fatalf("expected one slot, got %d", len(snap.Slots))
	}
	if got := snap.Slots[0].Health; got == HealthHealthy {
		t.Fatalf("a dead process must never be reported healthy (got %q)", got)
	}
	if snap.Status == StatusSucceeded {
		t.Fatalf("nothing is serving, so the deployment must not read as succeeded")
	}
	// No proxy client: routing is unknown, and unknown must not look like "off".
	if snap.Proxy != nil {
		t.Fatalf("without a proxy client the snapshot must omit proxy state, got %+v", snap.Proxy)
	}
}

func TestSnapshotForApp_Rolling_CountsAndUpstreams(t *testing.T) {
	resetHome(t)

	self := os.Getpid()
	exe := mustSelfExe(t)
	state := &DeployState{
		AppName: "rollapp", AppID: "8", Mode: ModeRolling, PublicPort: 3002,
		ActiveVersion: 9,
		Replicas: map[string]*Instance{
			"0": {Slot: "0", Version: 9, Status: "running", Port: 50000, PID: self, BinaryPath: exe},
			"1": {Slot: "1", Version: 9, Status: "running", Port: 50001, PID: self, BinaryPath: exe},
			// Recorded but dead: current counts it as absent, not ready/healthy.
			"2": {Slot: "2", Version: 8, Status: "running", Port: 50002, PID: 999999},
		},
		Health: &HealthSummary{Tier: 2, HealthyAt: time.Now()},
	}
	if err := Store(state); err != nil {
		t.Fatalf("store: %v", err)
	}

	pc := &statusOnlyProxy{statuses: []proxy.AppStatus{{
		AppName:    "rollapp",
		PublicPort: 3002,
		Primary:    proxy.Target{Host: "127.0.0.1:50000", Label: "replica-0"},
		Backends:   []proxy.Target{{Host: "127.0.0.1:50001", Label: "replica-1"}},
	}}}

	snap := SnapshotForApp(context.Background(), "rollapp", "8", pc)
	if snap == nil {
		t.Fatalf("expected a snapshot")
	}
	if snap.ReplicasDesired != 3 {
		t.Fatalf("desired = %d, want 3 (recorded replicas)", snap.ReplicasDesired)
	}
	if snap.ReplicasReady != 2 {
		t.Fatalf("ready = %d, want 2 live replicas", snap.ReplicasReady)
	}
	if snap.ReplicasHealthy != 2 {
		t.Fatalf("healthy = %d, want 2", snap.ReplicasHealthy)
	}
	if snap.ReplicasCurrent != 3 {
		t.Fatalf("current = %d, want 3 recorded PIDs", snap.ReplicasCurrent)
	}
	// Replica ids/indices are stable and ordered.
	for i, rep := range snap.Replicas {
		if rep.Index != i || rep.ID != replicaID(i) {
			t.Fatalf("replica %d = %+v, expected ordered replica-%d", i, rep, i)
		}
	}
	if snap.Replicas[2].Health == HealthHealthy {
		t.Fatalf("the dead replica must not report healthy: %+v", snap.Replicas[2])
	}
	if len(snap.Proxy.Upstreams) != 2 {
		t.Fatalf("upstreams = %v, want the primary plus one backend", snap.Proxy.Upstreams)
	}
}

func TestSnapshotForApp_ProxyDaemonUnreachable_LeavesProxyUnknown(t *testing.T) {
	resetHome(t)

	state := &DeployState{
		AppName: "noproxy", AppID: "10", Mode: ModeBlueGreen, PublicPort: 3003,
		ActiveSlot: SlotBlue,
		Slots:      map[string]*Instance{SlotBlue: {Slot: SlotBlue, Status: "stopped"}},
	}
	if err := Store(state); err != nil {
		t.Fatalf("store: %v", err)
	}

	pc := &statusOnlyProxy{err: context.DeadlineExceeded}
	snap := SnapshotForApp(context.Background(), "noproxy", "10", pc)
	if snap == nil {
		t.Fatalf("expected a snapshot")
	}
	if snap.Proxy != nil {
		t.Fatalf("an unreachable daemon must leave proxy unknown, got %+v", snap.Proxy)
	}
}

func TestSnapshotForApp_DaemonDoesNotKnowApp_ReportsNotProxied(t *testing.T) {
	resetHome(t)

	state := &DeployState{
		AppName: "unenrolled", AppID: "12", Mode: ModeBlueGreen, PublicPort: 3004,
		Slots: map[string]*Instance{SlotBlue: {Slot: SlotBlue, Status: "stopped"}},
	}
	if err := Store(state); err != nil {
		t.Fatalf("store: %v", err)
	}

	// Daemon answers, but has no route for this app.
	pc := &statusOnlyProxy{statuses: nil}
	snap := SnapshotForApp(context.Background(), "unenrolled", "12", pc)
	if snap.Proxy == nil {
		t.Fatalf("a reachable daemon should yield a definite proxy state")
	}
	if snap.Proxy.Enabled {
		t.Fatalf("app is not enrolled, so proxy must not be enabled: %+v", snap.Proxy)
	}
	if snap.Proxy.PublicPort != 3004 {
		t.Fatalf("public port = %d, want the persisted 3004", snap.Proxy.PublicPort)
	}
}

func TestSnapshotForApp_InFlightOperation_ReportsInProgress(t *testing.T) {
	resetHome(t)

	state := &DeployState{
		AppName: "busy", AppID: "13", Mode: ModeBlueGreen, PublicPort: 3005,
		Slots: map[string]*Instance{SlotBlue: {Slot: SlotBlue, Status: "starting"}},
	}
	if err := Store(state); err != nil {
		t.Fatalf("store: %v", err)
	}
	release, err := AcquireDeployLock("busy", "deploy")
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer release()

	snap := SnapshotForApp(context.Background(), "busy", "13", nil)
	if snap.Status != StatusInProgress {
		t.Fatalf("status = %q, want in_progress while an operation holds the lock", snap.Status)
	}
	if snap.Slots[0].Health != HealthPending {
		t.Fatalf("a starting instance is pending, got %q", snap.Slots[0].Health)
	}
}

func TestSnapshotForApp_StaleOpLockDoesNotReportInProgress(t *testing.T) {
	resetHome(t)
	state := &DeployState{
		AppName: "stale", AppID: "14", Mode: ModeBlueGreen, PublicPort: 3006,
		Slots:  map[string]*Instance{SlotBlue: {Slot: SlotBlue, Status: "stopped"}},
		OpLock: &DeployLock{Operation: "rollback", StartedAt: time.Now(), PID: 999999},
	}
	if err := Store(state); err != nil {
		t.Fatalf("store: %v", err)
	}
	snap := SnapshotForApp(context.Background(), "stale", "14", nil)
	if snap.Status == StatusInProgress {
		t.Fatalf("stale persisted OpLock must not report in_progress after owner exit")
	}
}

func TestSnapshotForApp_DockerRuntimeCopiesImageAndContainer(t *testing.T) {
	resetHome(t)

	// status "starting" keeps healthOf from routing through InstanceAlive (which
	// would shell out to Docker); this test is about the image/container/runtime
	// fields reaching the snapshot, not health.
	state := &DeployState{
		AppName: "billing", AppID: "20", Mode: ModeBlueGreen, PublicPort: 3010,
		Runtime:    "docker",
		ActiveSlot: SlotGreen,
		Slots: map[string]*Instance{
			SlotGreen: {
				Slot: SlotGreen, Version: 3, Status: "starting", Port: 49153,
				BinaryPath: "billing:v3", ContainerID: "c0ffee",
			},
		},
	}
	if err := Store(state); err != nil {
		t.Fatalf("store: %v", err)
	}

	snap := SnapshotForApp(context.Background(), "billing", "20", nil)
	if snap == nil {
		t.Fatalf("expected a snapshot")
	}
	if snap.Runtime != "docker" {
		t.Fatalf("runtime = %q, want docker", snap.Runtime)
	}
	if len(snap.Slots) != 1 {
		t.Fatalf("expected one slot, got %d", len(snap.Slots))
	}
	sl := snap.Slots[0]
	if sl.ContainerID != "c0ffee" || sl.Image != "billing:v3" {
		t.Fatalf("slot image/container = %q/%q, want billing:v3/c0ffee", sl.Image, sl.ContainerID)
	}
}

func TestSnapshotForApp_NativeLeavesImageAndContainerEmpty(t *testing.T) {
	resetHome(t)

	// A native instance: no ContainerID, empty Runtime, but BinaryPath IS set (a
	// filesystem path). Image must stay empty — the native binary path must never
	// leak into it — and empty means "unknown", never "native".
	state := &DeployState{
		AppName: "web", AppID: "21", Mode: ModeBlueGreen, PublicPort: 3011,
		ActiveSlot: SlotBlue,
		Slots: map[string]*Instance{
			SlotBlue: {
				Slot: SlotBlue, Version: 5, Status: "stopped", Port: 49150,
				BinaryPath: "/var/lib/phelix/apps/web/current",
			},
		},
	}
	if err := Store(state); err != nil {
		t.Fatalf("store: %v", err)
	}

	snap := SnapshotForApp(context.Background(), "web", "21", nil)
	if snap == nil {
		t.Fatalf("expected a snapshot")
	}
	if snap.Runtime != "" {
		t.Fatalf("native runtime must stay empty, got %q", snap.Runtime)
	}
	sl := snap.Slots[0]
	if sl.Image != "" || sl.ContainerID != "" {
		t.Fatalf("native slot must leave image/container empty (BinaryPath must not leak), got %q/%q", sl.Image, sl.ContainerID)
	}
}

// mustSelfExe returns this test binary's path, used as an Instance.BinaryPath so
// InstanceAlive's identity check passes for the current PID.
func mustSelfExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	return exe
}
