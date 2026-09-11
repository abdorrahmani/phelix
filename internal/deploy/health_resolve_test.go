package deploy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/health"
)

// pathHealth returns a tier config pinned to an explicit health path (Tier 1)
// with fast timings for tests.
func pathHealth(path string) *health.DeployTierConfig {
	return &health.DeployTierConfig{Mode: health.TierModeAuto, Path: path, Interval: "5ms", Retries: 2, Timeout: "3s"}
}

// pathHealthShort is pathHealth with a tiny overall timeout for the
// failure-path tests (they wait out the whole timeout by design).
func pathHealthShort(path string) *health.DeployTierConfig {
	cfg := pathHealth(path)
	cfg.Timeout = "500ms"
	return cfg
}

// pathAwareLauncher serves 200 only on healthyPath and 503 elsewhere. The
// distinction proves Tier 1 execution: Tier 2 would accept ANY HTTP response
// (including the 503s), while Tier 1 requires a 2xx on the configured path.
type pathAwareLauncher struct {
	mu          sync.Mutex
	healthyPath string
	servers     []*httptest.Server
}

func (pl *pathAwareLauncher) Launch(_ context.Context, _ string, _ []string) (Process, int, error) {
	pl.mu.Lock()
	healthy := pl.healthyPath
	pl.mu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == healthy {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux)
	pl.mu.Lock()
	pl.servers = append(pl.servers, srv)
	pl.mu.Unlock()
	return &selfProc{pid: os.Getpid()}, portOf(srv.URL), nil
}

func (pl *pathAwareLauncher) close() {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	for _, s := range pl.servers {
		s.Close()
	}
}

func portOf(url string) int {
	var port int
	fmt.Sscanf(url[len("http://127.0.0.1:"):], "%d", &port)
	return port
} // TestSelectDeployHealth_Tier1ConfiguredAndQuiet: a configured endpoint must
// select Tier 1 and must NOT emit the "No health endpoint configured"
// warning (regression: deploy claimed no endpoint existed).
func TestSelectDeployHealth_Tier1ConfiguredAndQuiet(t *testing.T) {
	log := &fakeLogger{}
	cfg := pathHealth("/health")
	tier := selectDeployHealth(context.Background(), cfg, "admin", "127.0.0.1:1", log, nil)
	if tier != health.Tier1HTTPPath {
		t.Fatalf("tier = %v, want Tier 1", tier)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.warns) != 0 {
		t.Fatalf("Tier 1 must not warn, got %v", log.warns)
	}
}

// TestSelectDeployHealth_NoConfigWarnsOnce: with genuinely no configuration
// the documented fallback warning still fires (Case E).
func TestSelectDeployHealth_NoConfigWarnsOnce(t *testing.T) {
	log := &fakeLogger{}
	// Nothing listening at this address → auto-detect lands on Tier 3 TCP.
	tier := selectDeployHealth(context.Background(), nil, "admin", "127.0.0.1:1", log, nil)
	if tier == health.Tier1HTTPPath {
		t.Fatalf("no config must not select Tier 1")
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.warns) != 1 {
		t.Fatalf("expected exactly one fallback warning, got %d: %v", len(log.warns), log.warns)
	}
}

// TestBlueGreen_UsesConfiguredHealthEndpoint (Case C): the persisted Tier 1
// config is actually executed — a 503 on the probed path aborts the deploy.
func TestBlueGreen_UsesConfiguredHealthEndpoint(t *testing.T) {
	resetHome(t)

	fl := &pathAwareLauncher{healthyPath: "/health"}
	defer fl.close()
	tierCfg := pathHealthShort("/nope") // /nope serves 503 → Tier 1 must fail

	bg := &BlueGreen{
		AppName: "tier1bg", AppID: "t1bg", PublicPort: 0,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    &fakeProxyClient{alive: true},
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return tierCfg },
		GracePeriod:    200 * time.Millisecond,
	}
	err := bg.Deploy(context.Background())
	if err == nil {
		t.Fatalf("Tier 1 check on a 503 path must abort the deploy")
	}

	// The failed (unproven) instance must be gone; nothing promoted.
	s := mustLoad(t, "tier1bg")
	if s.ActiveSlot != "" {
		t.Fatalf("aborted deploy must not flip the active slot, got %q", s.ActiveSlot)
	}
}

// TestBlueGreen_Tier1ConfiguredPathPasses: the same deploy with the healthy
// path configured succeeds and stays quiet (no fallback warning).
func TestBlueGreen_Tier1ConfiguredPathPasses(t *testing.T) {
	resetHome(t)

	fl := &pathAwareLauncher{healthyPath: "/health"}
	defer fl.close()
	log := &fakeLogger{}

	bg := &BlueGreen{
		AppName: "tier1ok", AppID: "t1ok", PublicPort: 0,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    &fakeProxyClient{alive: true},
		Logger:         log,
		HealthProvider: func(string) *health.DeployTierConfig { return pathHealth("/health") },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy with healthy Tier 1 path: %v", err)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, w := range log.warns {
		if strings.Contains(w, "No health endpoint configured") {
			t.Fatalf("Tier 1 deploy must not warn: %q", w)
		}
	}
}

// TestRolling_UsesConfiguredHealthEndpoint (Case D): rolling must resolve and
// execute the same configured endpoint — 503 on the configured path aborts.
func TestRolling_UsesConfiguredHealthEndpoint(t *testing.T) {
	resetHome(t)

	fl := &pathAwareLauncher{healthyPath: "/health"}
	defer fl.close()

	r := &Rolling{
		AppName: "tier1roll", AppID: "t1r", PublicPort: 8200, Replicas: 2,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    &fakeProxyClient{alive: true},
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return pathHealthShort("/nope") },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := r.Deploy(context.Background()); err == nil {
		t.Fatalf("rolling deploy must fail the Tier 1 check on a 503 path")
	}
}

func containsNoHealthEndpoint(s string) bool {
	return len(s) >= 4 && s[:4] == "⚠ No" || (len(s) > 25 && s[:25] == "⚠ No health endpoint conf")
}

// TestDefaultHealthProvider_ResolvesPersistedConfig (Case B): the provider
// used by both deploy modes must return the SAME configuration that
// `phelix health list/status` display — read back from the real on-disk
// location (~/.phelix/apps/<AppName>/health.json) in a fresh manager.
func TestDefaultHealthProvider_ResolvesPersistedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	appID := "ab034d93-6540-4870-b8e4-0a10fbc1f755"
	dir := filepath.Join(home, ".phelix", "apps", "admin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Raw JSON on purpose: pins the documented storage contract instead of
	// going through the writer under test.
	const json = `{
	  "app_id": "ab034d93-6540-4870-b8e4-0a10fbc1f755",
	  "app_name": "admin",
	  "endpoints": {
	    "default": {"name": "default", "url": "http://localhost:8085/api/v1/admin/health",
	                "interval": "10s", "retries": 3, "expectedCodes": "200-299", "timeout": "10s"}
	  },
	  "enabled": true,
	  "deploy_tier": {"mode": "auto", "path": "/api/v1/admin/health", "interval": "10s", "retries": 3, "timeout": "10s"}
	}`
	if err := os.WriteFile(filepath.Join(dir, "health.json"), []byte(json), 0o644); err != nil {
		t.Fatalf("write health.json: %v", err)
	}

	// Write BOTH fixtures before the first provider call: the manager scans
	// its directory once at initialization and never re-reads later.
	legacyID := "legacy-app-id"
	dir2 := filepath.Join(home, ".phelix", "apps", "legacyapp")
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const legacy = `{
	  "app_id": "legacy-app-id",
	  "app_name": "legacyapp",
	  "endpoints": {
	    "default": {"name": "default", "url": "http://localhost:9000/healthz", "interval": "10s", "retries": 3, "timeout": "10s"}
	  },
	  "enabled": true
	}`
	if err := os.WriteFile(filepath.Join(dir2, "health.json"), []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy health.json: %v", err)
	}

	provider := DefaultHealthProvider()
	got := provider(appID)
	if got == nil {
		t.Fatalf("deploy health provider returned nil for configured app (regression: deploy ignores persisted config)")
	}
	if got.Path != "/api/v1/admin/health" {
		t.Fatalf("provider Path = %q, want /api/v1/admin/health", got.Path)
	}

	got = provider(legacyID)
	if got == nil || got.Path != "/healthz" {
		t.Fatalf("legacy endpoint-only config must bridge to Tier 1 path, got %+v", got)
	}

	// Genuinely unconfigured app → nil (Tier 2/3 fallback is correct there).
	if got := provider("no-such-app"); got != nil {
		t.Fatalf("unconfigured app must resolve to nil, got %+v", got)
	}
}
