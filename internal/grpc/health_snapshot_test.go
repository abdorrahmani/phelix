package grpc

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/health"
)

const testAppID = "72b242bd-c1a9-44fc-9365-023976028e66"

// newSnapshot builds a snapshot with one probed and one never-probed endpoint.
func newSnapshot(checked time.Time) *health.AppHealthSnapshot {
	code, latency := 200, int64(12)
	return &health.AppHealthSnapshot{
		AppID:          testAppID,
		AppName:        "billing",
		Enabled:        true,
		Status:         health.StatusDegraded,
		UpdatedAt:      checked,
		DeployTier:     &health.DeployTierConfig{Mode: health.TierModeHTTP, Path: "/health"},
		EndpointsTotal: 2,
		EndpointsUp:    1,
		Endpoints: []health.EndpointHealthSnapshot{
			{
				Config: health.HealthCheckConfig{
					ID: "ep-1", Name: "default", URL: "http://localhost:8085/health",
					Interval: "10s", Retries: 3, ExpectedCodes: "200-299", Timeout: "10s",
				},
				Result:              &health.HealthCheckResult{Status: "UP", StatusCode: &code, LatencyMs: &latency, CheckedAt: checked},
				ConsecutiveFailures: 0,
				LastSuccess:         checked,
			},
			{
				// Configured but never probed: no result yet.
				Config: health.HealthCheckConfig{ID: "ep-2", Name: "readiness", URL: "http://localhost:8085/ready"},
			},
		},
	}
}

// The wire message must carry the app UUID and the stable endpoint identity, and
// must keep configuration and observation in separate sub-messages.
func TestToProtoAppHealthSnapshot(t *testing.T) {
	checked := time.Now().Truncate(time.Millisecond)
	got := toProtoAppHealthSnapshot(newSnapshot(checked))

	if got.GetAppId() != testAppID {
		t.Fatalf("AppId = %q, want the app UUID %q", got.GetAppId(), testAppID)
	}
	if got.GetAppName() != "billing" || !got.GetEnabled() || got.GetStatus() != health.StatusDegraded {
		t.Fatalf("header mismatch: %+v", got)
	}
	if got.GetUpdatedAt() != checked.UnixMilli() {
		t.Fatalf("UpdatedAt = %d, want %d", got.GetUpdatedAt(), checked.UnixMilli())
	}
	if got.GetEndpointsTotal() != 2 || got.GetEndpointsUp() != 1 {
		t.Fatalf("EndpointsTotal/Up = %d/%d, want 2/1", got.GetEndpointsTotal(), got.GetEndpointsUp())
	}
	if got.GetDeployTier().GetMode() != string(health.TierModeHTTP) || got.GetDeployTier().GetPath() != "/health" {
		t.Fatalf("DeployTier mismatch: %+v", got.GetDeployTier())
	}
	if len(got.GetEndpoints()) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(got.GetEndpoints()))
	}

	probed := got.GetEndpoints()[0]
	if probed.GetEndpointId() != "ep-1" {
		t.Fatalf("EndpointId = %q, want the stable id %q", probed.GetEndpointId(), "ep-1")
	}
	cfg := probed.GetConfig()
	if cfg.GetName() != "default" || cfg.GetUrl() != "http://localhost:8085/health" ||
		cfg.GetInterval() != "10s" || cfg.GetRetries() != 3 ||
		cfg.GetExpectedCodes() != "200-299" || cfg.GetTimeout() != "10s" {
		t.Fatalf("endpoint config not carried: %+v", cfg)
	}
	st := probed.GetStatus()
	if st.GetStatus() != "UP" || st.GetStatusCode() != 200 ||
		st.GetLatencyMs() != 12 || !st.GetLatencyMeasured() || st.GetCheckedAt() != checked.UnixMilli() {
		t.Fatalf("endpoint status not carried: %+v", st)
	}

	// An unprobed endpoint must carry its configuration but NO status message:
	// the backend keys "not yet checked" off the absent status, never off a
	// zeroed one that would look like a failure at epoch.
	unprobed := got.GetEndpoints()[1]
	if unprobed.GetEndpointId() != "ep-2" || unprobed.GetConfig().GetName() != "readiness" {
		t.Fatalf("unprobed endpoint lost its identity/config: %+v", unprobed)
	}
	if unprobed.Status != nil {
		t.Fatalf("unprobed endpoint must have no status message, got %+v", unprobed.GetStatus())
	}
}

// A timed-out probe has no latency to report. Without the flag, latency_ms 0
// would read as "0 ms" instead of "not measured".
func TestToProtoAppHealthSnapshot_TimeoutHasNoLatency(t *testing.T) {
	msg := "context deadline exceeded"
	snap := &health.AppHealthSnapshot{
		AppID:  testAppID,
		Status: health.StatusDown,
		Endpoints: []health.EndpointHealthSnapshot{{
			Config: health.HealthCheckConfig{ID: "ep-1", Name: "default"},
			Result: &health.HealthCheckResult{Status: "TIMEOUT", Error: &msg},
		}},
	}

	st := toProtoAppHealthSnapshot(snap).GetEndpoints()[0].GetStatus()
	if st.GetStatus() != "TIMEOUT" {
		t.Fatalf("Status = %q, want TIMEOUT", st.GetStatus())
	}
	if st.GetLatencyMeasured() || st.GetLatencyMs() != 0 {
		t.Fatalf("timeout must report no measured latency, got measured=%v ms=%d", st.GetLatencyMeasured(), st.GetLatencyMs())
	}
	if st.GetStatusCode() != 0 {
		t.Fatalf("StatusCode = %d, want 0 when no HTTP response was obtained", st.GetStatusCode())
	}
	if st.GetError() != msg {
		t.Fatalf("Error = %q, want %q", st.GetError(), msg)
	}
}

// An app configured with zero endpoints must serialize to an explicit empty set,
// not an omitted field: that is how deleting the last endpoint reaches the
// backend under replace-by-snapshot semantics.
func TestToProtoAppHealthSnapshot_EmptyEndpointsIsExplicit(t *testing.T) {
	got := toProtoAppHealthSnapshot(&health.AppHealthSnapshot{
		AppID:  testAppID,
		Status: health.StatusUnknown,
	})
	if got.Endpoints == nil {
		t.Fatal("Endpoints must be an empty list, not nil")
	}
	if len(got.GetEndpoints()) != 0 || got.GetEndpointsTotal() != 0 {
		t.Fatalf("expected zero endpoints, got %+v", got.GetEndpoints())
	}
}

// Serializing the same snapshot twice must produce identical bytes: retries and
// reconnect re-sends must be byte-for-byte idempotent so the backend can
// deduplicate or blindly re-apply without diverging.
func TestToProtoAppHealthSnapshot_Idempotent(t *testing.T) {
	snap := newSnapshot(time.Now().Truncate(time.Millisecond))

	first, second := toProtoAppHealthSnapshot(snap), toProtoAppHealthSnapshot(snap)
	if first.String() != second.String() {
		t.Fatalf("repeated serialization diverged:\n%s\n%s", first.String(), second.String())
	}
}

// No health daemon in this process means nothing is reported, rather than a
// panic from the singleton accessor.
func TestSendHealthSnapshots_NoDaemonIsNoop(t *testing.T) {
	if d := health.RunningDaemon(); d != nil {
		t.Skip("a health daemon is initialized in this process")
	}
	(&Client{}).sendHealthSnapshots()
}

// healthAppStub serves a fixed app list so the snapshot sender resolves the same
// UUID the backend is keyed on.
type healthAppStub struct {
	app.AppManagerInterface
	items []app.AppListItem
}

func (s *healthAppStub) ListApplications() []app.AppListItem { return s.items }
func (s *healthAppStub) LoadState() error                    { return nil }

// The load-bearing end-to-end property: a full health snapshot is pushed on
// EVERY stream open, not just the first, and it is a complete replacement. So a
// configuration change made while the connection was down — here a deletion,
// the case the old fire-and-forget RPC lost permanently — reaches the backend on
// reconnect without any event replay.
func TestMonitorStream_HealthSnapshotOnEveryConnectCarriesDeletes(t *testing.T) {
	setupTestSession(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	origMgr := app.Manager
	app.Manager = &healthAppStub{items: []app.AppListItem{{ID: testAppID, Name: "billing"}}}
	t.Cleanup(func() { app.Manager = origMgr })

	origCollector, origExecutor := monitorStream.metricsCollector, monitorStream.commandExecutor
	monitorStream.metricsCollector = fakeMetricsCollector{}
	monitorStream.commandExecutor = &fakeCommandExecutor{}
	origHealth := healthSnapshotInterval
	healthSnapshotInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		monitorStream.metricsCollector = origCollector
		monitorStream.commandExecutor = origExecutor
		healthSnapshotInterval = origHealth
	})

	cm, err := health.InitConfigManager()
	if err != nil {
		t.Fatalf("InitConfigManager: %v", err)
	}
	err = cm.SaveConfig(testAppID, &health.AppHealthConfig{
		AppID:   testAppID,
		AppName: "billing",
		Enabled: true,
		Endpoints: map[string]*health.HealthCheckConfig{
			"default": {Name: "default", URL: srv.URL, Interval: "10ms", Retries: 3, ExpectedCodes: "200-299", Timeout: "2s"},
		},
	})
	if err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	daemon, err := health.InitDaemon()
	if err != nil {
		t.Fatalf("InitDaemon: %v", err)
	}
	if err := daemon.Start(); err != nil {
		t.Fatalf("daemon Start: %v", err)
	}
	t.Cleanup(func() { _ = daemon.Stop() })

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	isHealth := func(ev *pb.MonitorEvent) bool {
		_, ok := ev.Payload.(*pb.MonitorEvent_AppHealth)
		return ok
	}
	runStream := func() func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = c.runMonitorStream()
		}()
		return func() {
			stopMonitorStream()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("timed out waiting for the monitor stream to end")
			}
		}
	}

	// First connection: the configured endpoint arrives with its identity.
	stop := runStream()
	ev := backend.eventsWithPayload(isHealth, 3*time.Second)
	if ev == nil {
		stop()
		t.Fatal("timed out waiting for an AppHealth event on the first connection")
	}
	snap := ev.GetAppHealth()
	if snap.GetAppId() != testAppID {
		t.Fatalf("AppId = %q, want %q", snap.GetAppId(), testAppID)
	}
	if len(snap.GetEndpoints()) != 1 {
		t.Fatalf("expected 1 endpoint on the first connection, got %+v", snap.GetEndpoints())
	}
	endpointID := snap.GetEndpoints()[0].GetEndpointId()
	if endpointID == "" {
		t.Fatal("endpoint reached the backend without a stable identity")
	}
	stop()

	// Connection is down. Remove the endpoint, exactly as `phelix health remove`
	// does, and let the daemon reconcile.
	cfg := cm.GetConfig(testAppID)
	delete(cfg.Endpoints, "default")
	if err := cm.SaveConfig(testAppID, cfg); err != nil {
		t.Fatalf("SaveConfig after removal: %v", err)
	}
	daemon.Sync()

	backend.mu.Lock()
	backend.received = nil
	backend.mu.Unlock()

	// Reconnect: the very next snapshot states the app has no endpoints, which is
	// how the backend learns to drop the one it still holds.
	stop = runStream()
	defer stop()
	ev = backend.eventsWithPayload(isHealth, 3*time.Second)
	if ev == nil {
		t.Fatal("no AppHealth event after reconnect: a reconnect must resync health")
	}
	if got := ev.GetAppHealth().GetEndpoints(); len(got) != 0 {
		t.Fatalf("reconnect still reports endpoints after removal: %+v", got)
	}
	if got := ev.GetAppHealth().GetStatus(); got != health.StatusUnknown {
		t.Fatalf("Status = %q, want %q with no endpoints left", got, health.StatusUnknown)
	}
}
