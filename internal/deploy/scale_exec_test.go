package deploy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
)

// newScaleFixture prepares an isolated HOME, a rolling deploy state with one
// running synthetic replica, and a recorded active version whose artifact
// exists on disk.
func newScaleFixture(t *testing.T, replicas int) (string, *DeployState) {
	t.Helper()
	resetHome(t)
	app := "scaleapp"
	rec, err := RecordFreshBuild(app, "app-"+app, writeScaleBinary(t), "", "", nil, DefaultRetention{Max: 5}, &nopLogger{})
	if err != nil {
		t.Fatalf("RecordFreshBuild: %v", err)
	}
	state, err := LoadOrInit(app, ModeRolling, 8080)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	state.ActiveVersion = rec.Version
	for i := 0; i < replicas; i++ {
		key := string(rune('0' + i))
		state.Replicas[key] = &Instance{
			Slot: key, PID: 999990 + i, Port: 19000 + i,
			Status: "running", Version: rec.Version,
		}
	}
	if err := Store(state); err != nil {
		t.Fatalf("Store: %v", err)
	}
	return app, state
}

func writeScaleBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	return path
}

func newScaleExecutor(app string, launcher InstanceLauncher, pc *fakeProxyClient, store func(*DeployState) error) *ScaleExecutor {
	return &ScaleExecutor{
		AppName: app, AppID: "", PublicPort: 8080,
		Launcher: launcher, ProxyClient: pc,
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		Logger:         &fakeLogger{},
		GracePeriod:    10 * time.Millisecond,
		stateStore:     store,
	}
}

func TestScaleExecutorScaleUp(t *testing.T) {
	app, _ := newScaleFixture(t, 1)
	pc := &fakeProxyClient{alive: true, enrolled: true}
	fl := &httpLauncher{}
	defer fl.close()

	ex := newScaleExecutor(app, fl.Launch, pc, nil)
	if err := ex.Scale(context.Background(), 1, 2); err != nil {
		t.Fatalf("Scale up: %v", err)
	}

	state := mustLoad(t, app)
	if got := len(state.Replicas); got != 2 {
		t.Fatalf("replicas = %d, want 2", got)
	}
	inst := state.Replicas["1"]
	if inst == nil || inst.Status != "running" || inst.Port == 0 {
		t.Fatalf("new replica = %+v, want running with a port", inst)
	}
	if inst.Version != state.ActiveVersion {
		t.Errorf("new replica version = %d, want active version %d (scaling must not create a version)", inst.Version, state.ActiveVersion)
	}
	if inst.BinaryPath == "" {
		t.Error("new replica has no binary path")
	}
	if state.ScaleProgress != nil {
		t.Errorf("scale marker left set: %+v", state.ScaleProgress)
	}
	if len(pc.switches) != 1 {
		t.Errorf("proxy switches = %d, want 1 membership update", len(pc.switches))
	}

	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatalf("LoadVersions: %v", err)
	}
	if len(vf.Versions) != 1 {
		t.Errorf("versions recorded = %d, want 1 (scaling must not rebuild)", len(vf.Versions))
	}
}

func TestScaleExecutorScaleDown(t *testing.T) {
	app, _ := newScaleFixture(t, 2)
	pc := &fakeProxyClient{alive: true, enrolled: true}

	ex := newScaleExecutor(app, nil, pc, nil)
	if err := ex.Scale(context.Background(), 2, 1); err != nil {
		t.Fatalf("Scale down: %v", err)
	}

	state := mustLoad(t, app)
	if got := len(state.Replicas); got != 1 {
		t.Fatalf("replicas = %d, want 1", got)
	}
	if _, ok := state.Replicas["1"]; ok {
		t.Error("highest-index replica should have been removed")
	}
	if state.ScaleProgress != nil {
		t.Errorf("scale marker left set: %+v", state.ScaleProgress)
	}
	if len(pc.switches) != 1 {
		t.Errorf("proxy switches = %d, want the exclusion membership update", len(pc.switches))
	}
}

func TestScaleExecutorRejectsStaleDecision(t *testing.T) {
	app, _ := newScaleFixture(t, 1)
	pc := &fakeProxyClient{alive: true, enrolled: true}

	ex := newScaleExecutor(app, nil, pc, nil)
	err := ex.Scale(context.Background(), 5, 6)
	if err == nil {
		t.Fatal("stale decision accepted")
	}
	if got := phelixerr.CodeOf(err); got != phelixerr.CodeInvalidArgument {
		t.Errorf("code = %v, want %v", got, phelixerr.CodeInvalidArgument)
	}
	state := mustLoad(t, app)
	if len(state.Replicas) != 1 {
		t.Errorf("replicas = %d, want untouched", len(state.Replicas))
	}
}

func TestScaleExecutorRejectsNonRolling(t *testing.T) {
	resetHome(t)
	app := "workerapp"
	if _, err := LoadOrInit(app, ModeWorker, 0); err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	ex := newScaleExecutor(app, nil, &fakeProxyClient{alive: true}, nil)
	if err := ex.Scale(context.Background(), 1, 2); err == nil {
		t.Fatal("worker app accepted by the rolling executor")
	}
}

func TestScaleExecutorDefersDuringCanary(t *testing.T) {
	app, state := newScaleFixture(t, 1)
	state.Canary = &CanaryState{Status: CanaryRunning}
	if err := Store(state); err != nil {
		t.Fatalf("Store: %v", err)
	}
	ex := newScaleExecutor(app, nil, &fakeProxyClient{alive: true, enrolled: true}, nil)
	if err := ex.Scale(context.Background(), 1, 2); err == nil {
		t.Fatal("scaling accepted during a canary rollout")
	}
}

func TestScaleExecutorRequiresActiveVersion(t *testing.T) {
	app, state := newScaleFixture(t, 1)
	state.ActiveVersion = 0
	if err := Store(state); err != nil {
		t.Fatalf("Store: %v", err)
	}
	ex := newScaleExecutor(app, nil, &fakeProxyClient{alive: true, enrolled: true}, nil)
	if err := ex.Scale(context.Background(), 1, 2); err == nil {
		t.Fatal("scale-up accepted without an active version")
	}
}

func TestScaleExecutorUnhealthyReplicaRestoresState(t *testing.T) {
	app, _ := newScaleFixture(t, 1)
	pc := &fakeProxyClient{alive: true, enrolled: true}

	ex := newScaleExecutor(app, closedLauncher{}.Launch, pc, nil)
	if err := ex.Scale(context.Background(), 1, 2); err == nil {
		t.Fatal("scale-up accepted an unhealthy replica")
	}

	state := mustLoad(t, app)
	if got := len(state.Replicas); got != 1 {
		t.Fatalf("replicas = %d, want the pre-scale fleet", got)
	}
	if state.ScaleProgress != nil {
		t.Errorf("scale marker left set after failure: %+v", state.ScaleProgress)
	}
	if len(pc.switches) != 0 {
		t.Errorf("proxy switches = %d, want none (unhealthy replica never entered rotation)", len(pc.switches))
	}
}

func TestScaleExecutorPersistFailureCompensatesProxy(t *testing.T) {
	app, _ := newScaleFixture(t, 1)
	pc := &fakeProxyClient{alive: true, enrolled: true}
	fl := &httpLauncher{}
	defer fl.close()

	calls := 0
	ex := newScaleExecutor(app, fl.Launch, pc, func(state *DeployState) error {
		calls++
		if calls == 3 {
			return phelixerr.New(phelixerr.CodeFilesystem, "injected commit failure")
		}
		return Store(state)
	})
	if err := ex.Scale(context.Background(), 1, 2); err == nil {
		t.Fatal("scale-up accepted despite the injected commit failure")
	}

	state := mustLoad(t, app)
	if got := len(state.Replicas); got != 1 {
		t.Fatalf("replicas = %d, want the pre-scale fleet after compensation", got)
	}
	if len(pc.switches) != 2 {
		t.Errorf("proxy switches = %d, want the forward switch plus the compensation switch", len(pc.switches))
	}
}

func TestScaleExecutorMarkerLifecycle(t *testing.T) {
	app, _ := newScaleFixture(t, 1)
	pc := &fakeProxyClient{alive: true, enrolled: true}
	fl := &httpLauncher{}
	defer fl.close()

	var markerSeen []*ScaleProgress
	ex := newScaleExecutor(app, fl.Launch, pc, func(state *DeployState) error {
		if state.ScaleProgress != nil {
			cp := *state.ScaleProgress
			markerSeen = append(markerSeen, &cp)
		}
		return Store(state)
	})
	if err := ex.Scale(context.Background(), 1, 2); err != nil {
		t.Fatalf("Scale up: %v", err)
	}
	if len(markerSeen) == 0 {
		t.Fatal("no persist observed the scale marker")
	}
	for _, m := range markerSeen {
		if m.Direction != "up" || m.From != 1 || m.To != 2 {
			t.Errorf("marker = %+v, want direction up from 1 to 2", m)
		}
	}
	state := mustLoad(t, app)
	if state.ScaleProgress != nil {
		t.Errorf("marker not cleared after completion: %+v", state.ScaleProgress)
	}
}

func TestRecoverInterruptedScaleUpCompleted(t *testing.T) {
	app, state := newScaleFixture(t, 2)
	state.ScaleProgress = &ScaleProgress{Direction: "up", From: 1, To: 2, StartedAt: time.Now()}
	if err := Store(state); err != nil {
		t.Fatalf("Store: %v", err)
	}

	if err := RecoverInterruptedScale(context.Background(), app, &fakeProxyClient{alive: true, enrolled: true}); err != nil {
		t.Fatalf("RecoverInterruptedScale: %v", err)
	}
	after := mustLoad(t, app)
	if after.ScaleProgress != nil {
		t.Errorf("marker not cleared: %+v", after.ScaleProgress)
	}
	if len(after.Replicas) != 2 {
		t.Errorf("replicas = %d, want the completed fleet untouched", len(after.Replicas))
	}
}

func TestRecoverInterruptedScaleUpOrphanStopped(t *testing.T) {
	app, state := newScaleFixture(t, 1)
	state.ScaleProgress = &ScaleProgress{Direction: "up", From: 1, To: 2, StartedAt: time.Now(), NewPID: 999998}
	if err := Store(state); err != nil {
		t.Fatalf("Store: %v", err)
	}

	if err := RecoverInterruptedScale(context.Background(), app, &fakeProxyClient{alive: true, enrolled: true}); err != nil {
		t.Fatalf("RecoverInterruptedScale: %v", err)
	}
	after := mustLoad(t, app)
	if after.ScaleProgress != nil {
		t.Errorf("marker not cleared: %+v", after.ScaleProgress)
	}
	if len(after.Replicas) != 1 {
		t.Errorf("replicas = %d, want the recorded fleet (orphan was never recorded)", len(after.Replicas))
	}
}

func TestRecoverInterruptedScaleDownVictimDrained(t *testing.T) {
	app, _ := newScaleFixture(t, 1)
	state := mustLoad(t, app)
	state.ScaleProgress = &ScaleProgress{
		Direction: "down", From: 2, To: 1, StartedAt: time.Now(),
		VictimKey: "1", VictimPID: 999997, VictimPort: 19001, VictimBinary: "",
	}
	if err := Store(state); err != nil {
		t.Fatalf("Store: %v", err)
	}

	if err := RecoverInterruptedScale(context.Background(), app, &fakeProxyClient{alive: true, enrolled: true}); err != nil {
		t.Fatalf("RecoverInterruptedScale: %v", err)
	}
	after := mustLoad(t, app)
	if after.ScaleProgress != nil {
		t.Errorf("marker not cleared: %+v", after.ScaleProgress)
	}
	if len(after.Replicas) != 1 {
		t.Errorf("replicas = %d, want the shrunken fleet", len(after.Replicas))
	}
}

func TestRecoverInterruptedScaleNoMarker(t *testing.T) {
	app, _ := newScaleFixture(t, 1)
	if err := RecoverInterruptedScale(context.Background(), app, nil); err != nil {
		t.Fatalf("RecoverInterruptedScale without marker: %v", err)
	}
}
