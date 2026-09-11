package deploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// --- rollout test fakes -----------------------------------------------------

// rolloutProxyClient records the full membership (primary + backends, with
// weights) of every Add/Switch, and serves scripted per-backend stats.
type rolloutProxyClient struct {
	mu         sync.Mutex
	alive      bool
	enrolled   bool
	failSwitch bool
	switches   [][]proxy.Target // each entry is the full membership in call order
	stats      [][]proxy.BackendStat
	statsErr   error
}

func (c *rolloutProxyClient) Ping(context.Context) error {
	if !c.alive {
		return errors.New("daemon not running")
	}
	return nil
}

func (c *rolloutProxyClient) record(primary proxy.Target, backends []proxy.Target) {
	set := append([]proxy.Target{primary}, backends...)
	c.switches = append(c.switches, set)
}

func (c *rolloutProxyClient) Add(_ context.Context, _ string, _ int, primary proxy.Target, backends ...proxy.Target) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record(primary, backends)
	c.enrolled = true
	return nil
}

func (c *rolloutProxyClient) Switch(_ context.Context, _ string, primary proxy.Target, backends ...proxy.Target) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failSwitch {
		return errors.New("switch rejected")
	}
	c.record(primary, backends)
	return nil
}

func (c *rolloutProxyClient) Remove(_ context.Context, _ string) error { return nil }

func (c *rolloutProxyClient) Status(_ context.Context, _ string) ([]proxy.AppStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enrolled {
		return nil, nil
	}
	return []proxy.AppStatus{{AppName: "app"}}, nil
}

// Stats implements the optional metrics extension; each call pops the next
// scripted snapshot so metric evaluation is deterministic.
func (c *rolloutProxyClient) Stats(_ context.Context, _ string) ([]proxy.BackendStat, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.statsErr != nil {
		return nil, c.statsErr
	}
	if len(c.stats) == 0 {
		return nil, nil
	}
	snap := c.stats[0]
	c.stats = c.stats[1:]
	return snap, nil
}

func (c *rolloutProxyClient) switchSets() [][]proxy.Target {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]proxy.Target, len(c.switches))
	copy(out, c.switches)
	return out
}

// lastSwitchSet is the most recent membership, or nil when nothing switched.
func (c *rolloutProxyClient) lastSwitchSet() []proxy.Target {
	sets := c.switchSets()
	if len(sets) == 0 {
		return nil
	}
	return sets[len(sets)-1]
}

// childProcInstance starts a real short-lived child process (sleep) and pairs
// it with a listening httptest server. The child's PID + resolved binary keep
// InstanceAlive truthful, while the server answers health probes on Port —
// and any drain/recovery kill targets the child, never this test process.
type childProcInstance struct {
	Instance
	server *httptest.Server
}

func startChildInstance(t *testing.T, slot string, ver int) *childProcInstance {
	t.Helper()
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep binary not available: %v", err)
	}
	cmd := exec.Command(sleepBin, "300")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start child process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return &childProcInstance{
		Instance: Instance{
			Slot: slot, PID: cmd.Process.Pid, Port: srv.Listener.Addr().(*net.TCPAddr).Port,
			BinaryPath: sleepBin, Status: "running", Version: ver,
		},
		server: srv,
	}
}

// seedStableBlueGreen persists a blue-green state whose active slot is a live
// instance backed by a real child process, so drains and recovery kills are
// observable and safe.
func seedStableBlueGreen(t *testing.T, appName string, ver int) *childProcInstance {
	t.Helper()
	stable := startChildInstance(t, SlotBlue, ver)
	state := &DeployState{
		AppName: appName, Mode: ModeBlueGreen, PublicPort: 8090,
		ActiveSlot: SlotBlue, ActiveVersion: ver,
		Slots: map[string]*Instance{
			SlotBlue:  &stable.Instance,
			SlotGreen: {Slot: SlotGreen, Status: "stopped"},
		},
	}
	if err := Store(state); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	return stable
}

func rolloutForTest(t *testing.T, appName string, steps []RolloutStep, pc *rolloutProxyClient, src BuildSource) *Rollout {
	t.Helper()
	return &Rollout{
		AppName: appName, AppID: "id-1", PublicPort: 8090,
		Plan:   RolloutPlan{Strategy: StrategyProgressive, Steps: steps},
		Source: src, Launcher: func(_ context.Context, _ string, _ []string) (Process, int, error) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, "ok")
			}))
			t.Cleanup(srv.Close)
			return &selfProc{pid: os.Getpid()}, srv.Listener.Addr().(*net.TCPAddr).Port, nil
		},
		ProxyClient:    pc,
		HealthProvider: func(string) *health.DeployTierConfig { return fastCfg() },
		Logger:         &fakeLogger{},
	}
}

// --- plan validation --------------------------------------------------------

func TestRolloutPlanValidate(t *testing.T) {
	ok := RolloutPlan{
		Strategy: StrategyProgressive,
		Steps:    []RolloutStep{{TrafficPercent: 5, Duration: time.Second}, {TrafficPercent: 100}},
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid plan rejected: %v", err)
	}
	bad := []struct {
		name string
		plan RolloutPlan
		want string
	}{
		{"no strategy", RolloutPlan{Steps: ok.Steps}, "strategy is required"},
		{"single step", RolloutPlan{Strategy: StrategyCanary, Steps: []RolloutStep{{TrafficPercent: 100}}}, "at least 2 steps"},
		{"zero traffic", RolloutPlan{Strategy: StrategyCanary, Steps: []RolloutStep{{TrafficPercent: 0}, {TrafficPercent: 100}}}, "between 1% and 100%"},
		{"traffic above 100", RolloutPlan{Strategy: StrategyCanary, Steps: []RolloutStep{{TrafficPercent: 101}, {TrafficPercent: 100}}}, "between 1% and 100%"},
		{"flat steps", RolloutPlan{Strategy: StrategyProgressive, Steps: []RolloutStep{{TrafficPercent: 5}, {TrafficPercent: 5}, {TrafficPercent: 100}}}, "strictly increase"},
		{"decreasing steps", RolloutPlan{Strategy: StrategyProgressive, Steps: []RolloutStep{{TrafficPercent: 50}, {TrafficPercent: 25}, {TrafficPercent: 100}}}, "strictly increase"},
		{"no final promotion", RolloutPlan{Strategy: StrategyProgressive, Steps: []RolloutStep{{TrafficPercent: 5}, {TrafficPercent: 50}}}, "final step must be the 100%"},
		{"negative duration", RolloutPlan{Strategy: StrategyProgressive, Steps: []RolloutStep{{TrafficPercent: 5, Duration: -time.Second}, {TrafficPercent: 100}}}, "negative duration"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.plan.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// --- engine: happy paths ----------------------------------------------------

// TestRollout_ProgressiveStepsAndPromotion walks a full 5%/25%/100% rollout
// and asserts every observable effect: the weighted switches in order, the
// final full cutover, the durable promotion (versions.json + active slot) and
// the drain of the old stable instance.
func TestRollout_ProgressiveStepsAndPromotion(t *testing.T) {
	resetHome(t)
	app := "roll-app"
	seedStableBlueGreen(t, app, 12)
	seedVersions(t, app, 12, 13)

	pc := &rolloutProxyClient{alive: true, enrolled: true}
	ro := rolloutForTest(t, app, []RolloutStep{
		{TrafficPercent: 5, Duration: 0},
		{TrafficPercent: 25, Duration: 0},
		{TrafficPercent: 100},
	}, pc, versionedSource{version: 13})
	// Default verifier: health-only here (no stats scripted → metrics skipped
	// after being marked unavailable on the first probe round).

	if err := ro.Deploy(context.Background()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	sets := pc.switchSets()
	if len(sets) != 3 {
		t.Fatalf("switch sets = %d, want 3 (one per step)", len(sets))
	}
	// Step switches carry the weighted split: primary=stable(100-p), canary(p).
	wantWeights := [][2]int{{95, 5}, {75, 25}}
	for i, w := range wantWeights {
		set := sets[i]
		if len(set) != 2 {
			t.Fatalf("step %d membership = %+v, want primary+canary", i, set)
		}
		if set[0].Weight != w[0] || set[1].Weight != w[1] {
			t.Errorf("step %d weights = %d/%d, want %d/%d", i, set[0].Weight, set[1].Weight, w[0], w[1])
		}
	}
	// The final switch is the full cutover: canary alone, no backends.
	final := sets[2]
	if len(final) != 1 || final[0].Label != SlotGreen {
		t.Errorf("final membership = %+v, want the canary slot alone", final)
	}

	state := mustLoad(t, app)
	if state.ActiveSlot != SlotGreen || state.ActiveVersion != 13 {
		t.Errorf("post-rollout state = slot %s v%d, want green v13", state.ActiveSlot, state.ActiveVersion)
	}
	if state.Canary == nil || state.Canary.Status != CanaryPromoted {
		t.Errorf("canary record = %+v, want status %q", state.Canary, CanaryPromoted)
	}
	// The old stable instance drained.
	if old := state.Slots[SlotBlue]; old == nil || old.PID != 0 || old.Status != "stopped" {
		t.Errorf("old stable slot = %+v, want drained/stopped", old)
	}
	// The canary instance is the new active and running.
	if green := state.Slots[SlotGreen]; green == nil || green.Status != "running" || green.Version != 13 {
		t.Errorf("canary slot = %+v, want running v13", green)
	}
	// Promotion is durable.
	if cur, err := CurrentVersion(app); err != nil || cur != 13 {
		t.Errorf("current version = %d (err %v), want 13", cur, err)
	}
}

// TestRollout_CanaryOneShotFromFlagPlan pins the plan shape behind
// `phelix rebuild --canary N`: hold at N% for the verification window, then
// promote.
func TestRollout_CanaryOneShotFromFlagPlan(t *testing.T) {
	resetHome(t)
	app := "canary-app"
	seedStableBlueGreen(t, app, 12)
	seedVersions(t, app, 12, 13)

	plan := CanaryPlan(5, 0, VerificationConfig{})
	if len(plan.Steps) != 2 || plan.Steps[0].TrafficPercent != 5 || plan.Steps[1].TrafficPercent != 100 {
		t.Fatalf("canary plan steps = %+v", plan.Steps)
	}

	pc := &rolloutProxyClient{alive: true, enrolled: true}
	ro := rolloutForTest(t, app, plan.Steps, pc, versionedSource{version: 13})
	ro.Plan = plan
	if err := ro.Deploy(context.Background()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	sets := pc.switchSets()
	if len(sets) != 2 {
		t.Fatalf("switch sets = %d, want 2", len(sets))
	}
	if sets[0][0].Weight != 95 || sets[0][1].Weight != 5 {
		t.Errorf("canary split = %+v, want 95/5", sets[0])
	}
}

// TestRollout_RollingMigrationAdoptsFirstReplica proves a rolling fleet can
// start a rollout: the first live replica becomes the blue (stable) slot, the
// rest retire after the first traffic switch, and the rollout proceeds.
func TestRollout_RollingMigrationAdoptsFirstReplica(t *testing.T) {
	resetHome(t)
	app := "migrate-app"
	r0 := startChildInstance(t, "0", 12)
	r1 := startChildInstance(t, "1", 12)
	state := &DeployState{
		AppName: app, Mode: ModeRolling, PublicPort: 8090, ActiveVersion: 12,
		Replicas: map[string]*Instance{"0": &r0.Instance, "1": &r1.Instance},
	}
	if err := Store(state); err != nil {
		t.Fatalf("seed rolling state: %v", err)
	}
	seedVersions(t, app, 12, 13)

	pc := &rolloutProxyClient{alive: true, enrolled: true}
	ro := rolloutForTest(t, app, []RolloutStep{{TrafficPercent: 10}, {TrafficPercent: 100}}, pc, versionedSource{version: 13})
	if err := ro.Deploy(context.Background()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	after := mustLoad(t, app)
	if after.Mode != ModeBlueGreen {
		t.Errorf("mode = %q, want blue-green after migration", after.Mode)
	}
	if after.ActiveSlot != SlotGreen || after.ActiveVersion != 13 {
		t.Errorf("state = slot %s v%d, want green v13", after.ActiveSlot, after.ActiveVersion)
	}
	if len(after.Replicas) != 0 {
		t.Errorf("replicas = %+v, want the fleet retired", after.Replicas)
	}
	// The adopted replica (blue) drained; the second replica retired with it.
	if blue := after.Slots[SlotBlue]; blue == nil || blue.PID != 0 {
		t.Errorf("adopted blue slot = %+v, want drained", blue)
	}
}

// --- engine: rejections -----------------------------------------------------

func TestRollout_RequiresExistingDeployment(t *testing.T) {
	resetHome(t)
	pc := &rolloutProxyClient{alive: true, enrolled: true}
	ro := rolloutForTest(t, "fresh-app", []RolloutStep{{TrafficPercent: 5}, {TrafficPercent: 100}}, pc, versionedSource{version: 2})
	err := ro.Deploy(context.Background())
	if err == nil {
		t.Fatal("expected an error for an app with no deployment")
	}
	if phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
		t.Errorf("code = %s, want INVALID_ARGUMENT", phelixerr.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "stable baseline") {
		t.Errorf("error %q should explain the missing stable baseline", err.Error())
	}
	if sets := pc.switchSets(); len(sets) != 0 {
		t.Errorf("switch sets = %d, want 0", len(sets))
	}
}

func TestRollout_RequiresLiveStableInstance(t *testing.T) {
	resetHome(t)
	app := "deadstable-app"
	// A blue-green state whose active slot recorded a dead PID.
	state := &DeployState{
		AppName: app, Mode: ModeBlueGreen, PublicPort: 8090, ActiveSlot: SlotBlue, ActiveVersion: 12,
		Slots: map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, PID: 999999999, Port: 1, Status: "running", Version: 12},
			SlotGreen: {Slot: SlotGreen, Status: "stopped"},
		},
	}
	if err := Store(state); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pc := &rolloutProxyClient{alive: true, enrolled: true}
	ro := rolloutForTest(t, app, []RolloutStep{{TrafficPercent: 5}, {TrafficPercent: 100}}, pc, versionedSource{version: 13})
	err := ro.Deploy(context.Background())
	if err == nil || phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
		t.Fatalf("err = %v, want INVALID_ARGUMENT", err)
	}
}

// TestRollout_HealthGateFailureNeverRoutesTraffic: a canary that cannot pass
// the deploy-tier health check is killed and the proxy is never touched.
func TestRollout_HealthGateFailureNeverRoutesTraffic(t *testing.T) {
	resetHome(t)
	app := "unhealthy-canary"
	seedStableBlueGreen(t, app, 12)

	pc := &rolloutProxyClient{alive: true, enrolled: true}
	ro := rolloutForTest(t, app, []RolloutStep{{TrafficPercent: 5}, {TrafficPercent: 100}}, pc, versionedSource{version: 13})
	// Launcher returns a port with nothing listening → WaitForHealthy fails.
	ro.Launcher = closedLauncher{}.Launch
	ro.HealthProvider = func(string) *health.DeployTierConfig { return fastHealthShortTimeout() }

	err := ro.Deploy(context.Background())
	if err == nil || phelixerr.CodeOf(err) != phelixerr.CodeHealthCheckFailed {
		t.Fatalf("err = %v, want HEALTH_CHECK_FAILED", err)
	}
	if sets := pc.switchSets(); len(sets) != 0 {
		t.Errorf("switch sets = %d, want 0 (traffic never moved)", len(sets))
	}
	state := mustLoad(t, app)
	if state.ActiveSlot != SlotBlue || state.ActiveVersion != 12 {
		t.Errorf("stable was disturbed: %+v", state)
	}
	if green := state.Slots[SlotGreen]; green == nil || green.Status != "failed" || green.PID != 0 {
		t.Errorf("failed canary slot = %+v, want failed with PID 0", green)
	}
}

// --- engine: failure handling -----------------------------------------------

// TestRollout_StepFailureRestoresStableTraffic is the core safety contract:
// a canary that fails verification mid-rollout leaves the stable version at
// 100% of traffic, the canary stopped, and the state describing reality.
func TestRollout_StepFailureRestoresStableTraffic(t *testing.T) {
	resetHome(t)
	app := "regress-app"
	stable := seedStableBlueGreen(t, app, 12)

	pc := &rolloutProxyClient{alive: true, enrolled: true}
	ro := rolloutForTest(t, app, []RolloutStep{
		{TrafficPercent: 5, Duration: 0},
		{TrafficPercent: 50, Duration: 0},
		{TrafficPercent: 100},
	}, pc, versionedSource{version: 13})
	verifierCalls := 0
	regression := phelixerr.New(phelixerr.CodeCanaryRegression, "canary regression detected: error rate baseline 0.31% vs canary 4.82%")
	ro.Verify = func(ctx context.Context, sc StepContext) error {
		verifierCalls++
		if sc.Step == 1 {
			return regression
		}
		return nil
	}

	err := ro.Deploy(context.Background())
	if err == nil || !errors.Is(err, regression) {
		t.Fatalf("err = %v, want the regression error", err)
	}
	if verifierCalls != 2 {
		t.Errorf("verifier calls = %d, want 2 (steps 0 and 1)", verifierCalls)
	}

	sets := pc.switchSets()
	if len(sets) != 3 {
		t.Fatalf("switch sets = %d, want 3 (step 0, step 1, restore)", len(sets))
	}
	restore := sets[2]
	if len(restore) != 1 || restore[0].Host != hostPort(stable.Port) {
		t.Errorf("restore membership = %+v, want the stable slot alone", restore)
	}

	state := mustLoad(t, app)
	if state.ActiveSlot != SlotBlue || state.ActiveVersion != 12 {
		t.Errorf("state after failure = slot %s v%d, want the stable baseline", state.ActiveSlot, state.ActiveVersion)
	}
	if state.Canary == nil || state.Canary.Status != CanaryFailed {
		t.Errorf("canary record = %+v, want status failed", state.Canary)
	}
	if green := state.Slots[SlotGreen]; green == nil || green.Status != "failed" || green.PID != 0 {
		t.Errorf("canary slot = %+v, want failed/stopped", green)
	}
	// The stable process survived the aborted rollout.
	if !InstanceAlive(&stable.Instance) {
		t.Error("stable instance did not survive the aborted rollout")
	}
}

// TestRollout_CancellationRestoresStableTraffic: Ctrl-C (context
// cancellation) between steps aborts the rollout through the same restore
// path, marking the rollout aborted rather than failed.
func TestRollout_CancellationRestoresStableTraffic(t *testing.T) {
	resetHome(t)
	app := "cancel-app"
	stable := seedStableBlueGreen(t, app, 12)

	pc := &rolloutProxyClient{alive: true, enrolled: true}
	ro := rolloutForTest(t, app, []RolloutStep{
		{TrafficPercent: 5, Duration: 0},
		{TrafficPercent: 100},
	}, pc, versionedSource{version: 13})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ro.Verify = func(ctx context.Context, sc StepContext) error {
		// Cancel while the first step is being verified.
		cancel()
		return ctx.Err()
	}

	err := ro.Deploy(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	restore := pc.lastSwitchSet()
	if len(restore) != 1 || restore[0].Host != hostPort(stable.Port) {
		t.Errorf("last membership = %+v, want the stable slot alone", restore)
	}
	state := mustLoad(t, app)
	if state.Canary == nil || state.Canary.Status != CanaryAborted {
		t.Errorf("canary record = %+v, want status aborted", state.Canary)
	}
	if state.ActiveSlot != SlotBlue {
		t.Errorf("active slot = %q, want the stable slot", state.ActiveSlot)
	}
}

// TestRollout_InterruptedRolloutRecovered: a rollout that crashed mid-flight
// (Canary record still "running") is recovered by the next rollout before any
// new traffic split is applied: stable routing first, stale canary killed.
func TestRollout_InterruptedRolloutRecovered(t *testing.T) {
	resetHome(t)
	app := "crash-app"
	stable := seedStableBlueGreen(t, app, 12)
	staleCanary := startChildInstance(t, SlotGreen, 13)

	state := mustLoad(t, app)
	state.Canary = &CanaryState{
		Version: 13, Strategy: StrategyProgressive, Slot: SlotGreen,
		Step: 0, Steps: 3, TrafficPercent: 25, Status: CanaryRunning,
	}
	state.Slots[SlotGreen] = &staleCanary.Instance
	if err := Store(state); err != nil {
		t.Fatalf("seed interrupted state: %v", err)
	}
	seedVersions(t, app, 12, 14)

	pc := &rolloutProxyClient{alive: true, enrolled: true}
	ro := rolloutForTest(t, app, []RolloutStep{{TrafficPercent: 10}, {TrafficPercent: 100}}, pc, versionedSource{version: 14})

	if err := ro.Deploy(context.Background()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// The very first switch the proxy saw was the recovery: stable alone.
	sets := pc.switchSets()
	if len(sets) < 3 {
		t.Fatalf("switch sets = %d, want >= 3 (recovery + 2 steps)", len(sets))
	}
	if first := sets[0]; len(first) != 1 || first[0].Host != hostPort(stable.Port) {
		t.Errorf("first switch = %+v, want the recovery restore to stable alone", first)
	}
	// The stale canary process was reclaimed.
	if InstanceAlive(&staleCanary.Instance) {
		t.Error("stale canary process from the interrupted rollout was not reclaimed")
	}
	after := mustLoad(t, app)
	if after.Canary == nil || after.Canary.Status != CanaryPromoted || after.ActiveVersion != 14 {
		t.Errorf("state after recovery rollout = %+v", after.Canary)
	}
}

// TestRollout_PromotionFailureCompensates: when the version promotion fails
// (unknown version), traffic goes back to stable and the canary stops.
func TestRollout_PromotionFailureCompensates(t *testing.T) {
	resetHome(t)
	app := "promote-fail-app"
	stable := seedStableBlueGreen(t, app, 12)
	// versions.json deliberately has no v13 → PromoteVersion fails.

	pc := &rolloutProxyClient{alive: true, enrolled: true}
	ro := rolloutForTest(t, app, []RolloutStep{{TrafficPercent: 5}, {TrafficPercent: 100}}, pc, versionedSource{version: 13})

	err := ro.Deploy(context.Background())
	if err == nil {
		t.Fatal("expected the promotion failure to surface")
	}
	if phelixerr.CodeOf(err) != phelixerr.CodeVersionNotFound {
		t.Errorf("code = %s, want VERSION_NOT_FOUND", phelixerr.CodeOf(err))
	}
	restore := pc.lastSwitchSet()
	if len(restore) != 1 || restore[0].Host != hostPort(stable.Port) {
		t.Errorf("last membership = %+v, want the stable slot alone", restore)
	}
	state := mustLoad(t, app)
	if state.ActiveSlot != SlotBlue || state.ActiveVersion != 12 {
		t.Errorf("state = slot %s v%d, want stable restored", state.ActiveSlot, state.ActiveVersion)
	}
	if green := state.Slots[SlotGreen]; green == nil || green.Status != "failed" || green.PID != 0 {
		t.Errorf("canary slot = %+v, want failed/stopped after compensation", green)
	}
}

// TestRollout_PersistFailureAtPromotionCompensates drives the state-store
// seam: a persistence failure at the promotion commit switches traffic back
// to stable instead of leaving the rollout half-promoted.
func TestRollout_PersistFailureAtPromotionCompensates(t *testing.T) {
	resetHome(t)
	app := "persist-fail-app"
	stable := seedStableBlueGreen(t, app, 12)
	seedVersions(t, app, 12, 13)

	pc := &rolloutProxyClient{alive: true, enrolled: true}
	ro := rolloutForTest(t, app, []RolloutStep{{TrafficPercent: 5}, {TrafficPercent: 100}}, pc, versionedSource{version: 13})
	ro.stateStore = func(s *DeployState) error {
		if s.Canary != nil && s.Canary.Status == CanaryPromoted {
			return errors.New("disk full")
		}
		return Store(s)
	}

	err := ro.Deploy(context.Background())
	if err == nil || phelixerr.CodeOf(err) != phelixerr.CodeFilesystem {
		t.Fatalf("err = %v, want FILESYSTEM_ERROR", err)
	}
	restore := pc.lastSwitchSet()
	if len(restore) != 1 || restore[0].Host != hostPort(stable.Port) {
		t.Errorf("last membership = %+v, want the stable slot alone", restore)
	}
	state := mustLoad(t, app)
	if state.ActiveSlot != SlotBlue || state.ActiveVersion != 12 {
		t.Errorf("state = slot %s v%d, want stable restored", state.ActiveSlot, state.ActiveVersion)
	}
}

// TestRollout_RunsUnderDeployLock pins the CLI wiring contract: the engine
// itself never acquires the per-app lock (the CLI does), so a rollout started
// while the lock is held by this process — exactly how runZeroDowntimeDeploy
// invokes it — must not deadlock.
func TestRollout_RunsUnderDeployLock(t *testing.T) {
	resetHome(t)
	app := "locked-app"
	seedStableBlueGreen(t, app, 12)
	seedVersions(t, app, 12, 13)

	release, err := AcquireDeployLock(app, "deploy")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	pc := &rolloutProxyClient{alive: true, enrolled: true}
	ro := rolloutForTest(t, app, []RolloutStep{{TrafficPercent: 5}, {TrafficPercent: 100}}, pc, versionedSource{version: 13})
	if err := ro.Deploy(context.Background()); err != nil {
		t.Fatalf("Deploy under held lock: %v", err)
	}
}
