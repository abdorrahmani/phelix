package deploy

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// Regression coverage for strategy migrations (classic/blue-green/rolling).

// versionedSource is a BuildSource with a known target version, so tests can
// assert version promotion and ActiveVersion synchronisation.
type versionedSource struct{ version int }

func (v versionedSource) Build(context.Context) (string, string, error) {
	return "/bin/true", "", nil
}
func (v versionedSource) Describe() string   { return "stub artifact v" }
func (v versionedSource) TargetVersion() int { return v.version }

// seedBlueGreenState persists a blue-green deployment whose blue slot "runs"
// (PID = the test process; BinaryPath deliberately mismatched so the identity
// check treats it as already exited instead of signalling the test binary).
func seedBlueGreenState(t *testing.T, appName string, version int) {
	t.Helper()
	state := &DeployState{
		AppName: appName, AppID: "bg", Mode: ModeBlueGreen, PublicPort: 8200,
		ActiveSlot:    SlotBlue,
		ActiveVersion: version,
		Slots: map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, PID: os.Getpid(), Port: 9001, BinaryPath: "/bin/true", Status: "running", Version: version},
			SlotGreen: {Slot: SlotGreen, Status: "stopped"},
		},
	}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	seedVersions(t, appName, version, version+1)
}

// seedVersions writes versions.json so PromoteVersion (which only promotes
// recorded versions) has rows to promote.
func seedVersions(t *testing.T, appName string, versions ...int) {
	t.Helper()
	vf := &VersionsFile{}
	for i, v := range versions {
		vf.Versions = append(vf.Versions, VersionMeta{Version: v, IsCurrent: i == 0, BuiltAt: time.Now()})
	}
	if err := saveVersions(appName, vf); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateTo_Unit(t *testing.T) {
	resetHome(t)

	// blue-green → rolling: blue is retired, slots cleared.
	s := &DeployState{AppName: "mig", Mode: ModeBlueGreen, ActiveSlot: SlotBlue,
		Slots: map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, PID: 4242, Status: "running"},
			SlotGreen: {Slot: SlotGreen, Status: "stopped"},
		}}
	retired, err := s.MigrateTo(ModeRolling)
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) != 1 || retired[0].PID != 4242 {
		t.Fatalf("retired = %+v, want only the running blue slot", retired)
	}
	if s.Mode != ModeRolling || s.Slots != nil || s.ActiveSlot != "" || s.Replicas == nil {
		t.Fatalf("post-migration state wrong: mode=%s slots=%v active=%q", s.Mode, s.Slots, s.ActiveSlot)
	}

	// rolling → blue-green: replicas retired, slot scaffolding created.
	s.Replicas["0"] = &Instance{Slot: "0", PID: 77, Status: "running"}
	retired, err = s.MigrateTo(ModeBlueGreen)
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) != 1 || retired[0].PID != 77 {
		t.Fatalf("retired = %+v, want the running replica", retired)
	}
	if s.Mode != ModeBlueGreen || s.Replicas != nil || s.Slots[SlotBlue] == nil || s.Slots[SlotGreen] == nil {
		t.Fatalf("post-migration state wrong: mode=%s replicas=%v", s.Mode, s.Replicas)
	}

	// Idempotent: a second MigrateTo on the same mode is a no-op.
	retired, err = s.MigrateTo(ModeBlueGreen)
	if err != nil || retired != nil {
		t.Fatalf("same-mode migrate returned (%v, %v), want nil/nil", retired, err)
	}
}

// Bug regression: rebuilding --replicas after a blue-green deploy used to keep
// mode=blue-green and the old slot records (and the old blue process) alive
// forever. The rollout must switch the recorded mode, clear the slots, and
// drain the blue instance once the first replica is in rotation.
func TestRolling_MigratesFromBlueGreen(t *testing.T) {
	resetHome(t)
	fl := &multiLauncher{}
	defer fl.close()
	seedBlueGreenState(t, "migrate-b2r", 4)

	ac := &auditedClient{
		inner:      &fakeProxyClient{alive: true},
		statusRows: []proxy.AppStatus{{AppName: "migrate-b2r"}},
	}
	r := &Rolling{
		AppName: "migrate-b2r", AppID: "mb", PublicPort: 8200, Replicas: 4,
		Source:         versionedSource{version: 5},
		Launcher:       fl.Launch,
		ProxyClient:    ac,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastCfg() },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := r.Deploy(context.Background()); err != nil {
		t.Fatalf("rolling migration deploy: %v", err)
	}

	st := mustLoad(t, "migrate-b2r")
	if st.Mode != ModeRolling {
		t.Fatalf("mode = %q, want rolling", st.Mode)
	}
	if st.Slots != nil || st.ActiveSlot != "" {
		t.Fatalf("stale blue-green state survived: slots=%v active=%q", st.Slots, st.ActiveSlot)
	}
	if len(st.Replicas) != 4 {
		t.Fatalf("replicas = %d, want 4", len(st.Replicas))
	}
	for k, inst := range st.Replicas {
		if inst.Status != "running" || inst.Port == 0 || inst.Version != 5 {
			t.Fatalf("replica %s wrong end state: %+v", k, inst)
		}
	}
	if st.ActiveVersion != 5 {
		t.Fatalf("ActiveVersion = %d, want 5", st.ActiveVersion)
	}
	if cur, err := CurrentVersion("migrate-b2r"); err != nil || cur != 5 {
		t.Fatalf("versions.json current = v%d (err %v), want v5", cur, err)
	}

	// The final membership update must cover exactly the 4 replicas: primary
	// plus backends, disjoint sets (the primary must never also appear inside
	// the backends — that is what old daemons persisted twice).
	ac.mu.Lock()
	snaps := ac.swapped
	ac.mu.Unlock()
	if len(snaps) != 4 {
		t.Fatalf("membership updates = %d, want 4", len(snaps))
	}
	final := snaps[len(snaps)-1]
	if len(final) < 2 {
		t.Fatalf("final membership = %+v, want primary + backends", final)
	}
	seenHosts := map[string]bool{}
	primHost := final[0].Host
	for _, m := range final {
		if seenHosts[m.Host] {
			t.Fatalf("duplicate backend host %q in final membership", m.Host)
		}
		seenHosts[m.Host] = true
	}
	if len(seenHosts) != 4 {
		t.Fatalf("final membership = %d hosts, want 4", len(seenHosts))
	}
	for _, m := range final[1:] {
		if m.Host == primHost {
			t.Fatalf("primary %q also listed among backends", primHost)
		}
	}
}

// A migration whose rollout fails before any replacement reached rotation must
// leave the previous strategy recorded and serving: blue-green state intact.
func TestRolling_MigrationFailureRestoresBlueGreen(t *testing.T) {
	resetHome(t)
	seedBlueGreenState(t, "migrate-fail", 4)

	r := &Rolling{
		AppName: "migrate-fail", AppID: "mf", PublicPort: 8200, Replicas: 4,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       closedLauncher{}.Launch,
		ProxyClient:    &fakeProxyClient{alive: true},
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealthShortTimeout() },
		GracePeriod:    100 * time.Millisecond,
	}
	if err := r.Deploy(context.Background()); err == nil {
		t.Fatal("migration deploy should fail (health check against closed port)")
	}
	st := mustLoad(t, "migrate-fail")
	if st.Mode != ModeBlueGreen || st.ActiveSlot != SlotBlue {
		t.Fatalf("failed migration did not restore blue-green: mode=%q active=%q", st.Mode, st.ActiveSlot)
	}
	if st.Slots == nil || st.Slots[SlotBlue] == nil || st.Slots[SlotBlue].PID != os.Getpid() || st.Slots[SlotBlue].Status != "running" {
		t.Fatalf("blue slot record not preserved: %+v", st.Slots)
	}
	if st.Replicas != nil {
		t.Fatalf("replica map left behind after restored migration: %+v", st.Replicas)
	}
}

// rolling → blue-green: replicas retire after the switch, slots take over.
func TestBlueGreen_MigratesFromRolling(t *testing.T) {
	resetHome(t)
	fl := &httpLauncher{}
	defer fl.close()

	state := &DeployState{
		AppName: "migrate-r2b", AppID: "mr", Mode: ModeRolling, PublicPort: 8201,
		ActiveVersion: 2, Replicas: map[string]*Instance{},
	}
	for _, k := range []string{"0", "1"} {
		state.Replicas[k] = &Instance{Slot: k, PID: os.Getpid(), Port: 9002,
			BinaryPath: "/bin/true", Status: "running", Version: 2}
	}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	seedVersions(t, "migrate-r2b", 2, 5)

	ac := &auditedClient{
		inner:      &fakeProxyClient{alive: true},
		statusRows: []proxy.AppStatus{{AppName: "migrate-r2b"}},
	}
	bg := &BlueGreen{
		AppName: "migrate-r2b", AppID: "mr", PublicPort: 8201,
		Source:         versionedSource{version: 5},
		Launcher:       fl.Launch,
		ProxyClient:    ac,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    200 * time.Millisecond,
		// The app is enrolled, so the public port belongs to the proxy itself;
		// a rolling → blue-green migration must never try a classic port
		// handoff (it would stop the app's own replicas and fail).
		PortHandoff: func(context.Context, string, int) error {
			t.Error("port handoff ran although the app is already enrolled")
			return nil
		},
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("blue-green migration deploy: %v", err)
	}
	st := mustLoad(t, "migrate-r2b")
	if st.Mode != ModeBlueGreen || st.Replicas != nil {
		t.Fatalf("stale rolling state survived: mode=%q replicas=%v", st.Mode, st.Replicas)
	}
	if st.ActiveSlot != SlotBlue || st.Slots == nil {
		t.Fatalf("blue-green state wrong after migration: active=%q slots=%v", st.ActiveSlot, st.Slots)
	}
	if inst := st.Slots[SlotBlue]; inst == nil || inst.Status != "running" || inst.Version != 5 {
		t.Fatalf("blue slot wrong end state: %+v", inst)
	}
	if st.ActiveVersion != 5 {
		t.Fatalf("ActiveVersion = %d, want 5", st.ActiveVersion)
	}
}

// A blue-green deploy that fails before the proxy switch must undo the
// migration so the state again describes the rolling deployment still serving.
func TestBlueGreen_MigrationFailureRestoresRolling(t *testing.T) {
	resetHome(t)
	fl := &httpLauncher{}
	defer fl.close()

	state := &DeployState{
		AppName: "migrate-r2b-fail", AppID: "mrf", Mode: ModeRolling, PublicPort: 8202,
		ActiveVersion: 2, Replicas: map[string]*Instance{
			"0": {Slot: "0", PID: os.Getpid(), Port: 9003, BinaryPath: "/bin/true", Status: "running", Version: 2},
			"1": {Slot: "1", PID: os.Getpid(), Port: 9004, BinaryPath: "/bin/true", Status: "running", Version: 2},
		},
	}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}

	// No proxy client: the deploy passes its health check, then aborts at the
	// enrolment step — before any traffic moved.
	bg := &BlueGreen{
		AppName: "migrate-r2b-fail", AppID: "mrf", PublicPort: 8202,
		Source:         versionedSource{version: 5},
		Launcher:       fl.Launch,
		ProxyClient:    nil,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := bg.Deploy(context.Background()); err == nil {
		t.Fatal("deploy should fail without a proxy client")
	}
	st := mustLoad(t, "migrate-r2b-fail")
	if st.Mode != ModeRolling || st.Replicas == nil || len(st.Replicas) != 2 {
		t.Fatalf("failed migration did not restore rolling state: mode=%q replicas=%v", st.Mode, st.Replicas)
	}
	if inst := st.Replicas["0"]; inst.Status != "running" || inst.Version != 2 {
		t.Fatalf("replica 0 not preserved: %+v", inst)
	}
}

// Idempotence: reconcile the same rolling membership twice through the daemon
// client path and the target set stays exactly the desired replicas (the
// duplicate replica-0 regression). Verified against the real Proxy.
func TestProxyMembershipReconcileIdempotent(t *testing.T) {
	p := proxy.New("idem", 8300, proxy.Target{Host: "127.0.0.1:1", Label: "replica-0"})
	want := []proxy.Target{
		{Host: "127.0.0.1:1", Label: "replica-0"},
		{Host: "127.0.0.1:2", Label: "replica-1"},
		{Host: "127.0.0.1:3", Label: "replica-2"},
	}
	// The rolling deploy convention passes the primary inside the backend set.
	members := append([]proxy.Target{want[0]}, want...)
	for i := 0; i < 3; i++ {
		p.SetTarget(want[0], members[1:]...)
		p.SetTarget(want[0], members...) // primary duplicated in addl
	}
	got := p.Targets()
	if len(got) != len(want) {
		t.Fatalf("targets = %+v, want exactly %d unique backends", got, len(want))
	}
	for i, t2 := range want {
		if got[i].Host != t2.Host || got[i].Label != t2.Label {
			t.Fatalf("targets[%d] = %+v, want %+v", i, got[i], t2)
		}
	}
}
