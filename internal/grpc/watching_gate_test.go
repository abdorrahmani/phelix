package grpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/health"
)

// watchingGateStub is an app-manager stub whose watching states can be flipped
// mid-test, standing in for `phelix watch`/`phelix watch --disable` run in
// another process while the monitor daemon keeps streaming.
type watchingGateStub struct {
	app.AppManagerInterface
	mu    sync.Mutex
	items []app.AppListItem
}

func (s *watchingGateStub) ListApplications() []app.AppListItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]app.AppListItem, len(s.items))
	copy(out, s.items)
	return out
}
func (s *watchingGateStub) LoadState() error { return nil }
func (s *watchingGateStub) StatusApplication(id string) (app.AppStatus, error) {
	return app.AppStatus{ID: id, Status: "stopped"}, nil
}

func (s *watchingGateStub) setWatching(id string, watching bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].Watching = watching
		}
	}
}

const (
	watchedAppID   = "11111111-1111-4111-8111-111111111111"
	unwatchedAppID = "22222222-2222-4222-8222-222222222222"
)

func newWatchingGateStub() *watchingGateStub {
	return &watchingGateStub{items: []app.AppListItem{
		{ID: unwatchedAppID, Name: "unwatched-app", Status: "stopped"},
		{ID: watchedAppID, Name: "watched-app", Status: "stopped", Watching: true},
	}}
}

// collectAppHealthIDs returns the app IDs of every AppHealth event the backend
// received so far.
func collectAppHealthIDs(b *fakeMonitorBackend) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for _, ev := range b.received {
		if h, ok := ev.Payload.(*pb.MonitorEvent_AppHealth); ok {
			ids = append(ids, h.AppHealth.GetAppId())
		}
	}
	return ids
}

// The core watching contract on the health channel: an unwatched app with a
// fully configured health check sends NO snapshot — not an empty one — while a
// watched app reports normally. Flipping watching at runtime (as `phelix watch
// --disable` does from another process) stops the watched app's stream on the
// next tick, and a reconnect must not resurrect reporting for unwatched apps.
func TestMonitorStream_HealthSnapshots_SkipUnwatchedApps(t *testing.T) {
	setupTestSession(t)

	// Local health checks need a live endpoint to probe; both apps get one so
	// the only difference under test is the watching flag.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stub := newWatchingGateStub()
	origMgr := app.Manager
	app.Manager = stub
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
	for _, id := range []string{watchedAppID, unwatchedAppID} {
		err := cm.SaveConfig(id, &health.AppHealthConfig{
			AppID:   id,
			AppName: id,
			Enabled: true,
			Endpoints: map[string]*health.HealthCheckConfig{
				"default": {Name: "default", URL: srv.URL, Interval: "10ms", Retries: 3, ExpectedCodes: "200-299", Timeout: "2s"},
			},
		})
		if err != nil {
			t.Fatalf("SaveConfig(%s): %v", id, err)
		}
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

	isWatchedHealth := func(ev *pb.MonitorEvent) bool {
		h, ok := ev.Payload.(*pb.MonitorEvent_AppHealth)
		return ok && h.AppHealth.GetAppId() == watchedAppID
	}

	// Phase 1: watched app reports; unwatched app never does, across at least
	// two full snapshot ticks.
	stop := runStream()
	if ev := backend.eventsWithPayload(isWatchedHealth, 3*time.Second); ev == nil {
		stop()
		t.Fatal("timed out waiting for the watched app's health snapshot")
	}
	backend.mu.Lock()
	backend.received = nil
	backend.mu.Unlock()
	if ev := backend.eventsWithPayload(isWatchedHealth, 3*time.Second); ev == nil {
		stop()
		t.Fatal("timed out waiting for the second health tick")
	}
	for _, id := range collectAppHealthIDs(backend) {
		if id == unwatchedAppID {
			stop()
			t.Fatalf("unwatched app's health snapshot reached the backend: %v", collectAppHealthIDs(backend))
		}
	}
	stop()

	// Phase 2: disable watching at runtime, reconnect, and verify the stream
	// goes silent — the reconnect path must re-check watching, not replay the
	// snapshot set captured at connect time.
	stub.setWatching(watchedAppID, false)
	backend.mu.Lock()
	backend.received = nil
	backend.mu.Unlock()

	stop = runStream()
	defer stop()
	time.Sleep(300 * time.Millisecond) // >10 snapshot intervals
	if ids := collectAppHealthIDs(backend); len(ids) != 0 {
		t.Fatalf("health snapshots after watching was disabled: %v", ids)
	}
}

// The deployment topology resync (connect + 60s ticker) must skip unwatched
// apps even when they have a zero-downtime deployment on disk.
func TestMonitorStream_DeploymentSnapshots_SkipUnwatchedApps(t *testing.T) {
	setupTestSession(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	for _, name := range []string{"watched-app", "unwatched-app"} {
		dir := filepath.Join(home, ".phelix", "apps", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		state := `{"app_name":"` + name + `","mode":"blue-green","public_port":8080}`
		if err := os.WriteFile(filepath.Join(dir, "deploy.json"), []byte(state), 0o644); err != nil {
			t.Fatalf("write deploy.json for %s: %v", name, err)
		}
	}

	origMgr := app.Manager
	app.Manager = newWatchingGateStub()
	t.Cleanup(func() { app.Manager = origMgr })

	origCollector, origExecutor := monitorStream.metricsCollector, monitorStream.commandExecutor
	monitorStream.metricsCollector = fakeMetricsCollector{}
	monitorStream.commandExecutor = &fakeCommandExecutor{}
	t.Cleanup(func() {
		monitorStream.metricsCollector = origCollector
		monitorStream.commandExecutor = origExecutor
	})

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.runMonitorStream()
	}()
	defer func() {
		stopMonitorStream()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("timed out waiting for the monitor stream to end")
		}
	}()

	// Deployment snapshots are pushed once per connection; give both apps the
	// chance to arrive before asserting.
	deadline := time.Now().Add(2 * time.Second)
	var ids []string
	for time.Now().Before(deadline) {
		backend.mu.Lock()
		ids = nil
		for _, ev := range backend.received {
			if d, ok := ev.Payload.(*pb.MonitorEvent_DeploymentSnapshot); ok {
				ids = append(ids, d.DeploymentSnapshot.GetAppName())
			}
		}
		backend.mu.Unlock()
		if len(ids) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(ids) != 1 || ids[0] != "watched-app" {
		t.Fatalf("deployment snapshots = %v, want exactly [watched-app]", ids)
	}
}

// recordingAutoRestartBackend records ReportAutoRestart calls.
type recordingAutoRestartBackend struct {
	pb.UnimplementedPhelixServiceServer
	mu    sync.Mutex
	calls []*pb.ReportAutoRestartRequest
}

func (b *recordingAutoRestartBackend) ReportAutoRestart(_ context.Context, req *pb.ReportAutoRestartRequest) (*pb.ReportAutoRestartResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, req)
	return &pb.ReportAutoRestartResponse{Success: true}, nil
}

func (b *recordingAutoRestartBackend) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

// Auto-restart is a monitoring-generated event: unwatched apps must not
// produce one, and skipping it is a success (the restart already happened
// locally), not an error.
func TestSendAutoRestartEvent_SkipsUnwatchedApps(t *testing.T) {
	setupTestSession(t)

	origMgr := app.Manager
	app.Manager = newWatchingGateStub()
	t.Cleanup(func() { app.Manager = origMgr })

	backend := &recordingAutoRestartBackend{}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	reporter := NewGrpcHealthReporter(c)
	record := &health.AutoRestartRecord{AppID: unwatchedAppID, AppName: "unwatched-app", Reason: "exit"}
	if err := reporter.SendAutoRestartEvent(record); err != nil {
		t.Fatalf("unwatched auto-restart report must be a silent skip, got error: %v", err)
	}
	if n := backend.callCount(); n != 0 {
		t.Fatalf("unwatched app sent %d auto-restart events, want 0", n)
	}

	record = &health.AutoRestartRecord{AppID: watchedAppID, AppName: "watched-app", Reason: "exit"}
	if err := reporter.SendAutoRestartEvent(record); err != nil {
		t.Fatalf("watched auto-restart report failed: %v", err)
	}
	if n := backend.callCount(); n != 1 {
		t.Fatalf("watched app sent %d auto-restart events, want 1", n)
	}
}

// recordingEventBackend records ReportEvent calls.
type recordingEventBackend struct {
	pb.UnimplementedPhelixServiceServer
	mu    sync.Mutex
	calls []*pb.ApplicationEvent
}

func (b *recordingEventBackend) ReportEvent(_ context.Context, ev *pb.ApplicationEvent) (*pb.EventResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, ev)
	return &pb.EventResponse{Accepted: true}, nil
}

func (b *recordingEventBackend) appIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.calls))
	for i, c := range b.calls {
		out[i] = c.GetAppId()
	}
	return out
}

// Lifecycle events (start/stop/...) are dashboard app-status reports: they
// are skipped for unwatched apps — silently, since the opt-out is intentional
// and not a staleness condition to warn about. Unresolvable apps fail open so
// `phelix remove`'s tombstone still cleans up a previously watched app.
func TestReportEventResult_WatchingGate(t *testing.T) {
	setupTestSession(t)

	origMgr := app.Manager
	app.Manager = newWatchingGateStub()
	t.Cleanup(func() { app.Manager = origMgr })

	backend := &recordingEventBackend{}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	origClient := GetClient()
	SetClient(c)
	t.Cleanup(func() { SetClient(origClient) })

	if err := ReportEventResult(unwatchedAppID, "unwatched-app", "stop", true, "", 0, "", ""); err != nil {
		t.Fatalf("unwatched event must be a silent skip, got error: %v", err)
	}
	if err := ReportEventResult(watchedAppID, "watched-app", "stop", true, "", 0, "", ""); err != nil {
		t.Fatalf("watched event failed: %v", err)
	}
	if err := ReportEventResult("gone-app", "gone-app", "remove", true, "", 0, "", ""); err != nil {
		t.Fatalf("tombstone event for an unresolvable app must still be sent, got error: %v", err)
	}

	got := backend.appIDs()
	want := []string{watchedAppID, "gone-app"}
	if len(got) != len(want) {
		t.Fatalf("reported events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reported events = %v, want %v", got, want)
		}
	}
}

// The periodic version sync enumerates only watched apps with a directory.
func TestWatchedAppsWithDirectory(t *testing.T) {
	origMgr := app.Manager
	app.Manager = &watchingGateStub{items: []app.AppListItem{
		{ID: "unwatched-dir", Name: "unwatched-dir", Directory: "/tmp/a", Watching: false},
		{ID: "watched-dir", Name: "watched-dir", Directory: "/tmp/b", Watching: true},
		{ID: "watched-nodir", Name: "watched-nodir", Directory: "", Watching: true},
	}}
	t.Cleanup(func() { app.Manager = origMgr })

	got := watchedAppsWithDirectory()
	if len(got) != 1 || got[0].ID != "watched-dir" {
		t.Fatalf("watchedAppsWithDirectory = %+v, want exactly watched-dir", got)
	}
}
