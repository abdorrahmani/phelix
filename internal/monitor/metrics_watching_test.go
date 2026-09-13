package monitor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/logs"
)

// watchingAppStub serves a fixed app list with mixed watching states; the
// app-manager resolution by ID is stubbed so no real process state is touched.
type watchingAppStub struct {
	app.AppManagerInterface
	items []app.AppListItem
}

func (s *watchingAppStub) ListApplications() []app.AppListItem { return s.items }
func (s *watchingAppStub) LoadState() error                    { return nil }
func (s *watchingAppStub) StatusApplication(id string) (app.AppStatus, error) {
	return app.AppStatus{ID: id, Name: id, Status: "stopped"}, nil
}

// mixedApps is the canonical mixed-watching scenario: A unwatched, B watched,
// C unwatched. Only B's data may reach the backend.
func mixedApps() []app.AppListItem {
	return []app.AppListItem{
		{ID: "app-a", Name: "alpha", Status: "stopped", Watching: false},
		{ID: "app-b", Name: "bravo", Status: "stopped", Watching: true},
		{ID: "app-c", Name: "charlie", Status: "stopped", Watching: false},
	}
}

func swapManagerForWatchingTest(t *testing.T, items []app.AppListItem) {
	t.Helper()
	orig := app.Manager
	app.Manager = &watchingAppStub{items: items}
	t.Cleanup(func() { app.Manager = orig })
}

// The 2s metrics tick must carry resource metrics for watched apps only.
func TestCollectAppMetrics_SkipsUnwatchedApps(t *testing.T) {
	swapManagerForWatchingTest(t, mixedApps())

	var got []string
	for _, m := range NewMetricsCollector().CollectAppMetrics() {
		got = append(got, m.AppID)
	}
	if len(got) != 1 || got[0] != "app-b" {
		t.Fatalf("CollectAppMetrics app IDs = %v, want exactly [app-b]", got)
	}
}

// ApplicationInfo (the dashboard's app view) must exclude unwatched apps.
func TestCollectAppDetails_SkipsUnwatchedApps(t *testing.T) {
	swapManagerForWatchingTest(t, mixedApps())

	details := NewMetricsCollector().CollectAppDetails()
	if len(details) != 1 || details[0].ID != "app-b" {
		t.Fatalf("CollectAppDetails app IDs = %+v, want exactly app-b", details)
	}
}

// App log collection must resolve log targets for watched apps only — an
// unwatched app's log tail must never be shipped, even when its log file
// exists and has fresh content.
func TestCollectAppLogs_SkipsUnwatchedApps(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dataDir)
	swapManagerForWatchingTest(t, mixedApps())

	logsDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	for _, id := range []string{"app-a", "app-b", "app-c"} {
		path := logs.AppLogPath(id)
		if err := os.WriteFile(path, []byte("line for "+id+"\n"), 0o644); err != nil {
			t.Fatalf("write log for %s: %v", id, err)
		}
	}

	entries, err := NewMetricsCollector().CollectAppLogs()
	if err != nil {
		t.Fatalf("CollectAppLogs: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected the watched app's log line to be collected")
	}
	for _, e := range entries {
		if e.AppID != "app-b" {
			t.Fatalf("collected log for unwatched app %q; only app-b may be reported: %+v", e.AppID, entries)
		}
	}
}
