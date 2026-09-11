package health

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
)

// appStub serves a fixed app list so the daemon resolves the same UUID the
// backend is keyed on.
type appStub struct {
	app.AppManagerInterface
	items []app.AppListItem
}

func (s *appStub) ListApplications() []app.AppListItem { return s.items }
func (s *appStub) LoadState() error                    { return nil }

// newTestDaemon gives each test its own daemon and config manager rooted in a
// temp HOME, with app.Manager stubbed to the given apps.
func newTestDaemon(t *testing.T, items ...app.AppListItem) *Daemon {
	t.Helper()
	resetConfigManagerForTest(t)

	orig := app.Manager
	app.Manager = &appStub{items: items}
	t.Cleanup(func() { app.Manager = orig })

	cm, err := InitConfigManager()
	if err != nil {
		t.Fatalf("InitConfigManager: %v", err)
	}
	d := &Daemon{
		endpointStates:  make(map[string]map[string]*EndpointState),
		running:         make(map[string]*checkHandle),
		checker:         NewChecker(),
		configManager:   cm,
		stopChan:        make(chan struct{}),
		broadcastChan:   make(chan *HealthCheckResult, 100),
		autoRestartChan: make(chan *AutoRestartRecord, 50),
	}
	t.Cleanup(func() {
		d.mu.Lock()
		running := d.isRunning
		d.mu.Unlock()
		if running {
			_ = d.Stop()
		}
	})
	return d
}

const testAppID = "72b242bd-c1a9-44fc-9365-023976028e66"

// saveEndpoints is the shorthand every test below uses to write a health
// configuration the way `phelix health set/add` does.
func saveEndpoints(t *testing.T, d *Daemon, endpoints map[string]*HealthCheckConfig) *AppHealthConfig {
	t.Helper()
	cfg := &AppHealthConfig{
		AppID:     testAppID,
		AppName:   "billing",
		Enabled:   true,
		Endpoints: endpoints,
	}
	if err := d.configManager.SaveConfig(testAppID, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	return cfg
}

func endpoint(name, url string) *HealthCheckConfig {
	return &HealthCheckConfig{Name: name, URL: url, Interval: "10s", Retries: 3, ExpectedCodes: "200-299", Timeout: "2s"}
}

// An app with no health config produces no snapshot at all: the backend must be
// able to tell "health not configured" from "configured but unhealthy".
func TestSnapshotForApp_NilWithoutConfig(t *testing.T) {
	d := newTestDaemon(t, app.AppListItem{ID: testAppID, Name: "billing"})

	if snap := d.SnapshotForApp(testAppID, "billing"); snap != nil {
		t.Fatalf("expected no snapshot for an app without health config, got %+v", snap)
	}
}

// The snapshot must be keyed by the app's UUID, not its name, must carry the
// endpoint's stable identity, and an endpoint that has not been probed yet must
// report UNKNOWN rather than DOWN.
func TestSnapshotForApp_UsesAppUUIDAndUnknownBeforeFirstCheck(t *testing.T) {
	d := newTestDaemon(t, app.AppListItem{ID: testAppID, Name: "billing"})
	saveEndpoints(t, d, map[string]*HealthCheckConfig{
		"default": endpoint("default", "http://127.0.0.1:1/health"),
	})

	snap := d.SnapshotForApp(testAppID, "billing")
	if snap == nil {
		t.Fatal("expected a snapshot once health is configured")
	}
	if snap.AppID != testAppID {
		t.Fatalf("snapshot AppID = %q, want the app UUID %q", snap.AppID, testAppID)
	}
	if snap.Status != StatusUnknown {
		t.Fatalf("Status = %q, want %q before the first check completes", snap.Status, StatusUnknown)
	}
	if snap.EndpointsTotal != 1 || snap.EndpointsUp != 0 {
		t.Fatalf("EndpointsTotal/Up = %d/%d, want 1/0", snap.EndpointsTotal, snap.EndpointsUp)
	}
	if len(snap.Endpoints) != 1 || snap.Endpoints[0].Result != nil {
		t.Fatalf("expected one endpoint with no result yet, got %+v", snap.Endpoints)
	}
	if snap.Endpoints[0].Config.ID == "" {
		t.Fatal("endpoint reached the snapshot without a stable identity")
	}
}

// The root-cause fix: health configured AFTER the daemon started must begin
// being checked and reported without restarting the daemon. `phelix health set`
// runs in a separate process, so the daemon only learns about it by re-reading
// the configs.
func TestSync_StartsCheckingConfigAddedAfterStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDaemon(t, app.AppListItem{ID: testAppID, Name: "billing"})

	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	d.mu.RLock()
	running := len(d.running)
	d.mu.RUnlock()
	if running != 0 {
		t.Fatalf("expected no checkers before any config exists, got %d", running)
	}

	// Simulate `phelix health set` writing the config from another process.
	ep := endpoint("default", srv.URL)
	ep.Interval = "10ms"
	saveEndpoints(t, d, map[string]*HealthCheckConfig{"default": ep})

	d.Sync()

	d.mu.RLock()
	running = len(d.running)
	d.mu.RUnlock()
	if running != 1 {
		t.Fatalf("expected 1 checker after Sync picked up the new config, got %d", running)
	}

	// The checker must actually report a result, which is what reaches the
	// backend snapshot.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("no health result within 3s of the config being added")
		case result := <-d.broadcastChan:
			if result.AppID != testAppID {
				t.Fatalf("result AppID = %q, want the app UUID %q", result.AppID, testAppID)
			}
			if result.Status != "UP" {
				t.Fatalf("result Status = %q, want UP", result.Status)
			}
			snap := d.SnapshotForApp(testAppID, "billing")
			if snap == nil || snap.Status != StatusUp {
				t.Fatalf("snapshot after a successful check = %+v, want status %q", snap, StatusUp)
			}
			return
		}
	}
}

// Removing the last endpoint stops its checker AND still produces a snapshot —
// an empty endpoint list. Reporting nothing would leave the backend holding an
// orphaned endpoint forever, because replace-by-snapshot is the only way a
// deletion travels.
func TestSync_StopsCheckerWhenConfigRemoved(t *testing.T) {
	d := newTestDaemon(t, app.AppListItem{ID: testAppID, Name: "billing"})
	saveEndpoints(t, d, map[string]*HealthCheckConfig{
		"default": endpoint("default", "http://127.0.0.1:1/health"),
	})
	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	d.mu.RLock()
	running := len(d.running)
	d.mu.RUnlock()
	if running != 1 {
		t.Fatalf("expected 1 checker after Start, got %d", running)
	}

	saveEndpoints(t, d, map[string]*HealthCheckConfig{})
	d.Sync()

	d.mu.RLock()
	running = len(d.running)
	states := len(d.endpointStates[testAppID])
	d.mu.RUnlock()
	if running != 0 {
		t.Fatalf("expected the checker to stop once its config was removed, got %d", running)
	}
	if states != 0 {
		t.Fatalf("expected the removed endpoint's observations to be dropped, got %d", states)
	}

	snap := d.SnapshotForApp(testAppID, "billing")
	if snap == nil {
		t.Fatal("an app whose endpoints were all removed must still report a snapshot")
	}
	if len(snap.Endpoints) != 0 || snap.EndpointsTotal != 0 {
		t.Fatalf("expected zero endpoints after removal, got %+v", snap.Endpoints)
	}
	if snap.Status != StatusUnknown {
		t.Fatalf("Status = %q, want %q with nothing left to probe", snap.Status, StatusUnknown)
	}
}

// Deleting ONE of several endpoints must remove exactly that one and leave the
// others' identities untouched.
func TestSync_DeletingOneEndpointKeepsTheOthers(t *testing.T) {
	d := newTestDaemon(t, app.AppListItem{ID: testAppID, Name: "billing"})
	saveEndpoints(t, d, map[string]*HealthCheckConfig{
		"default":   endpoint("default", "http://127.0.0.1:1/health"),
		"readiness": endpoint("readiness", "http://127.0.0.1:1/ready"),
	})

	before := map[string]string{}
	for _, e := range d.SnapshotForApp(testAppID, "billing").Endpoints {
		before[e.Config.Name] = e.Config.ID
	}
	if len(before) != 2 {
		t.Fatalf("expected 2 endpoints, got %v", before)
	}

	cfg := d.configManager.GetConfig(testAppID)
	delete(cfg.Endpoints, "readiness")
	if err := d.configManager.SaveConfig(testAppID, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	after := d.SnapshotForApp(testAppID, "billing").Endpoints
	if len(after) != 1 || after[0].Config.Name != "default" {
		t.Fatalf("expected only 'default' to survive, got %+v", after)
	}
	if after[0].Config.ID != before["default"] {
		t.Fatalf("surviving endpoint changed identity: %q -> %q", before["default"], after[0].Config.ID)
	}
}

// Sync must be idempotent: a second call with an unchanged configuration must
// leave the existing checkers alone rather than tearing them down and losing
// their accumulated failure counters.
func TestSync_IdempotentForUnchangedConfig(t *testing.T) {
	d := newTestDaemon(t, app.AppListItem{ID: testAppID, Name: "billing"})
	saveEndpoints(t, d, map[string]*HealthCheckConfig{
		"default": endpoint("default", "http://127.0.0.1:1/health"),
	})
	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	d.mu.RLock()
	first := d.running[checkerKey(testAppID, "default")]
	d.mu.RUnlock()

	d.Sync()
	d.Sync()

	d.mu.RLock()
	second, count := d.running[checkerKey(testAppID, "default")], len(d.running)
	d.mu.RUnlock()
	if count != 1 {
		t.Fatalf("repeated Sync changed the checker count to %d, want 1", count)
	}
	if first != second {
		t.Fatal("repeated Sync replaced an unchanged checker")
	}
}

// Editing an endpoint's configuration must replace its checker so the new
// settings take effect, while keeping its identity.
func TestSync_ReplacesCheckerWhenConfigEdited(t *testing.T) {
	d := newTestDaemon(t, app.AppListItem{ID: testAppID, Name: "billing"})
	saveEndpoints(t, d, map[string]*HealthCheckConfig{
		"default": endpoint("default", "http://127.0.0.1:1/health"),
	})
	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	d.mu.RLock()
	before := d.running[checkerKey(testAppID, "default")]
	d.mu.RUnlock()

	cfg := d.configManager.GetConfig(testAppID)
	id := cfg.Endpoints["default"].ID
	cfg.Endpoints["default"].URL = "http://127.0.0.1:2/health"
	if err := d.configManager.SaveConfig(testAppID, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	d.Sync()

	d.mu.RLock()
	after, count := d.running[checkerKey(testAppID, "default")], len(d.running)
	d.mu.RUnlock()
	if count != 1 {
		t.Fatalf("checker count = %d, want 1", count)
	}
	if after == before {
		t.Fatal("edited configuration did not restart the checker")
	}
	if after.cfg.URL != "http://127.0.0.1:2/health" {
		t.Fatalf("checker still probing %q", after.cfg.URL)
	}
	if got := d.SnapshotForApp(testAppID, "billing").Endpoints[0].Config.ID; got != id {
		t.Fatalf("an edit changed the endpoint identity: %q -> %q", id, got)
	}
}

// `phelix build` rebuilds the whole endpoint map from phelix.yaml on every
// build. Identity has to survive that, or every rebuild would look to the
// backend like "delete all endpoints, create new ones".
func TestSaveConfig_IdentitySurvivesWholesaleReplace(t *testing.T) {
	d := newTestDaemon(t, app.AppListItem{ID: testAppID, Name: "billing"})
	saveEndpoints(t, d, map[string]*HealthCheckConfig{
		"default": endpoint("default", "http://127.0.0.1:1/health"),
	})
	first := d.configManager.GetConfig(testAppID).Endpoints["default"].ID

	// A fresh config object with fresh endpoint objects, exactly as
	// syncProjectHealth builds it.
	saveEndpoints(t, d, map[string]*HealthCheckConfig{
		"default": endpoint("default", "http://127.0.0.1:1/health"),
	})
	second := d.configManager.GetConfig(testAppID).Endpoints["default"].ID

	if first == "" || first != second {
		t.Fatalf("endpoint identity not preserved across a wholesale replace: %q -> %q", first, second)
	}
}

// Identity must survive an agent restart, which reads health.json back from disk.
func TestConfigManager_IdentitySurvivesReload(t *testing.T) {
	d := newTestDaemon(t, app.AppListItem{ID: testAppID, Name: "billing"})
	saveEndpoints(t, d, map[string]*HealthCheckConfig{
		"default": endpoint("default", "http://127.0.0.1:1/health"),
	})
	before := d.configManager.GetConfig(testAppID).Endpoints["default"].ID

	// Simulate a fresh process reading the same directory.
	configMgr = nil
	fresh, err := InitConfigManager()
	if err != nil {
		t.Fatalf("InitConfigManager: %v", err)
	}
	after := fresh.GetConfig(testAppID).Endpoints["default"].ID
	if before == "" || before != after {
		t.Fatalf("endpoint identity not stable across a restart: %q -> %q", before, after)
	}
}

// A health.json written before identities existed gets one minted on load and
// persisted, so it is stable from then on rather than changing every restart.
func TestConfigManager_MintsIdentityForLegacyConfig(t *testing.T) {
	resetConfigManagerForTest(t)

	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".phelix", "apps", "legacyapp")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	legacy := `{"app_id":"legacy-id","app_name":"legacyapp","enabled":true,"endpoints":{` +
		`"default":{"name":"default","url":"http://localhost:8085/healthz","interval":"10s","retries":3}}}`
	if err := os.WriteFile(filepath.Join(dir, "health.json"), []byte(legacy), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cm, err := InitConfigManager()
	if err != nil {
		t.Fatalf("InitConfigManager: %v", err)
	}
	minted := cm.GetConfig("legacy-id").Endpoints["default"].ID
	if minted == "" {
		t.Fatal("legacy endpoint did not get an identity")
	}

	configMgr = nil
	reloaded, err := InitConfigManager()
	if err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if got := reloaded.GetConfig("legacy-id").Endpoints["default"].ID; got != minted {
		t.Fatalf("minted identity was not persisted: %q -> %q", minted, got)
	}
}

// GetConfig must hand out a copy: the monitor process reads configs on the
// health daemon's goroutine while backend commands mutate them on the agent
// stream's, and sharing the cached pointer made that a data race.
func TestGetConfig_ReturnsIndependentCopy(t *testing.T) {
	d := newTestDaemon(t, app.AppListItem{ID: testAppID, Name: "billing"})
	saveEndpoints(t, d, map[string]*HealthCheckConfig{
		"default": endpoint("default", "http://127.0.0.1:1/health"),
	})

	mine := d.configManager.GetConfig(testAppID)
	mine.Endpoints["default"].URL = "http://mutated/"
	mine.Endpoints["scratch"] = endpoint("scratch", "http://scratch/")

	fresh := d.configManager.GetConfig(testAppID)
	if fresh.Endpoints["default"].URL != "http://127.0.0.1:1/health" {
		t.Fatalf("mutating a returned config leaked into the cache: %q", fresh.Endpoints["default"].URL)
	}
	if _, added := fresh.Endpoints["scratch"]; added {
		t.Fatal("adding to a returned config's map leaked into the cache")
	}
}

// The rollup must walk UNKNOWN -> UP -> DOWN -> UP as observations arrive, and
// report DEGRADED while endpoints disagree.
func TestSnapshotForApp_RollupTransitions(t *testing.T) {
	d := newTestDaemon(t, app.AppListItem{ID: testAppID, Name: "billing"})
	saveEndpoints(t, d, map[string]*HealthCheckConfig{
		"default":   endpoint("default", "http://127.0.0.1:1/health"),
		"readiness": endpoint("readiness", "http://127.0.0.1:1/ready"),
	})

	observe := func(name, status string) {
		cfg := d.configManager.GetConfig(testAppID).Endpoints[name]
		d.updateEndpointState(testAppID, "billing", name, cfg,
			&HealthCheckResult{EndpointName: name, Status: status, CheckedAt: time.Now()}, nil)
	}
	statusNow := func() string { return d.SnapshotForApp(testAppID, "billing").Status }

	if got := statusNow(); got != StatusUnknown {
		t.Fatalf("Status = %q, want UNKNOWN before any probe", got)
	}
	observe("default", "UP")
	if got := statusNow(); got != StatusUp {
		t.Fatalf("Status = %q, want UP with one probed endpoint up", got)
	}
	observe("readiness", "DOWN")
	if got := statusNow(); got != StatusDegraded {
		t.Fatalf("Status = %q, want DEGRADED while endpoints disagree", got)
	}
	observe("default", "TIMEOUT")
	if got := statusNow(); got != StatusDown {
		t.Fatalf("Status = %q, want DOWN with nothing up", got)
	}
	observe("default", "UP")
	observe("readiness", "UP")
	snap := d.SnapshotForApp(testAppID, "billing")
	if snap.Status != StatusUp || snap.EndpointsUp != 2 {
		t.Fatalf("recovery not reflected: status=%q up=%d", snap.Status, snap.EndpointsUp)
	}
	// Recovery must clear the failure counter the backend reads.
	for _, e := range snap.Endpoints {
		if e.ConsecutiveFailures != 0 {
			t.Fatalf("%s still reports %d consecutive failures after recovery", e.Config.Name, e.ConsecutiveFailures)
		}
	}
}

// Building the same snapshot twice must produce equal content: reconnect
// re-sends and retries must not make the backend see a change that did not
// happen.
func TestSnapshotForApp_StableAcrossRepeatedBuilds(t *testing.T) {
	d := newTestDaemon(t, app.AppListItem{ID: testAppID, Name: "billing"})
	saveEndpoints(t, d, map[string]*HealthCheckConfig{
		"b-second": endpoint("b-second", "http://127.0.0.1:1/two"),
		"a-first":  endpoint("a-first", "http://127.0.0.1:1/one"),
	})

	first, second := d.SnapshotForApp(testAppID, "billing"), d.SnapshotForApp(testAppID, "billing")
	if len(first.Endpoints) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(first.Endpoints))
	}
	// Endpoint order must be deterministic, or every re-send would look like a
	// reordering to a backend that compares payloads.
	if first.Endpoints[0].Config.Name != "a-first" || first.Endpoints[1].Config.Name != "b-second" {
		t.Fatalf("endpoints not in stable name order: %+v", first.Endpoints)
	}
	for i := range first.Endpoints {
		if first.Endpoints[i].Config != second.Endpoints[i].Config {
			t.Fatalf("endpoint %d differed between builds: %+v vs %+v",
				i, first.Endpoints[i].Config, second.Endpoints[i].Config)
		}
	}
	if first.Status != second.Status || first.EndpointsTotal != second.EndpointsTotal {
		t.Fatalf("rollup differed between builds: %+v vs %+v", first, second)
	}
}

func TestRollupStatus(t *testing.T) {
	cases := []struct {
		checked, up int
		want        string
	}{
		{0, 0, StatusUnknown},
		{2, 2, StatusUp},
		{2, 0, StatusDown},
		{3, 1, StatusDegraded},
	}
	for _, c := range cases {
		if got := rollupStatus(c.checked, c.up); got != c.want {
			t.Fatalf("rollupStatus(%d, %d) = %q, want %q", c.checked, c.up, got, c.want)
		}
	}
}

// RunningDaemon must be nil until a daemon is actually running, so a plain CLI
// process reports nothing instead of panicking on the singleton.
func TestRunningDaemon_NilUntilStarted(t *testing.T) {
	orig := daemonPtr.Load()
	t.Cleanup(func() { daemonPtr.Store(orig) })
	daemonPtr.Store(nil)

	if got := RunningDaemon(); got != nil {
		t.Fatalf("RunningDaemon() = %+v with no daemon, want nil", got)
	}

	d := newTestDaemon(t, app.AppListItem{ID: testAppID, Name: "billing"})
	daemonPtr.Store(d)
	if got := RunningDaemon(); got != nil {
		t.Fatal("RunningDaemon() must stay nil for an initialized-but-stopped daemon")
	}
	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := RunningDaemon(); got != d {
		t.Fatalf("RunningDaemon() = %+v after Start, want the daemon", got)
	}
}

// The synchronization layer runs three things against the same configuration at
// once in the monitor process: the daemon reconciling checkers, the gRPC sender
// building snapshots, and CLI/backend commands rewriting health.json. Run under
// -race, this pins that they do not share mutable state.
func TestConcurrentSyncSnapshotAndSave(t *testing.T) {
	d := newTestDaemon(t, app.AppListItem{ID: testAppID, Name: "billing"})
	saveEndpoints(t, d, map[string]*HealthCheckConfig{
		"default": endpoint("default", "http://127.0.0.1:1/health"),
	})
	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); d.Sync() }()
		go func() {
			defer wg.Done()
			if snap := d.SnapshotForApp(testAppID, "billing"); snap == nil {
				t.Error("snapshot disappeared while the configuration was being rewritten")
			}
		}()
		go func(n int) {
			defer wg.Done()
			cfg := d.configManager.GetConfig(testAppID)
			if cfg == nil {
				t.Error("config disappeared mid-run")
				return
			}
			cfg.Endpoints["default"].Timeout = fmt.Sprintf("%ds", n+1)
			if err := d.configManager.SaveConfig(testAppID, cfg); err != nil {
				t.Errorf("SaveConfig: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// Whatever interleaving won, exactly one endpoint is configured and it kept
	// its identity.
	snap := d.SnapshotForApp(testAppID, "billing")
	if len(snap.Endpoints) != 1 || snap.Endpoints[0].Config.ID == "" {
		t.Fatalf("configuration ended up inconsistent: %+v", snap.Endpoints)
	}
}
