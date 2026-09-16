package app

import (
	"os"
	"path/filepath"
	"testing"
)

// isolateStateFile points the package-level state file at a temp directory.
// stateFile is bound at package init (before any t.Setenv can take effect),
// so in-package tests must rebind it explicitly to avoid touching the real
// ~/.phelix.
func isolateStateFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldStateFile, oldLogDir := stateFile, logDir
	stateFile = filepath.Join(dir, "apps.json")
	logDir = filepath.Join(dir, "logs")
	t.Cleanup(func() {
		stateFile = oldStateFile
		logDir = oldLogDir
	})
	return dir
}

// A state file written before the watching field existed (no "watching" key)
// must load as Watching=false: an upgrade never opts an existing app into
// backend monitoring.
func TestLoadState_WatchingDefaultsDisabledForLegacyState(t *testing.T) {
	isolateStateFile(t)

	legacy := `{
  "203": {
    "id": "203",
    "name": "demo",
    "pid": 0,
    "status": "stopped",
    "port": 8080,
    "directory": "/tmp/demo",
    "language": "go",
    "no_upload": false,
    "auto_start": false
  }
}`
	if err := os.WriteFile(stateFile, []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy apps.json: %v", err)
	}

	m := &AppManager{}
	if err := m.LoadState(); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	info, ok := m.Apps["203"]
	if !ok {
		t.Fatal("app 203 not loaded")
	}
	if info.Watching {
		t.Fatal("legacy app without watching field must load as Watching=false")
	}

	// The repair-pass save must also round-trip the default without enabling
	// it: reload the persisted file and re-check.
	m2 := &AppManager{}
	if err := m2.LoadState(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if m2.Apps["203"].Watching {
		t.Fatal("save/reload flipped Watching to true")
	}
}

// Watching must survive save/load round-trips in both states — a CLI restart,
// machine restart, or monitor restart must not lose the user's choice.
func TestWatching_PersistsAcrossSaveLoad(t *testing.T) {
	isolateStateFile(t)

	for _, watching := range []bool{true, false} {
		m := &AppManager{Apps: map[string]*AppInfo{
			"203": {ID: "203", Name: "demo", Status: "stopped", Watching: watching},
		}}
		if err := m.SaveState(); err != nil {
			t.Fatalf("SaveState(watching=%v): %v", watching, err)
		}

		reloaded := &AppManager{}
		if err := reloaded.LoadState(); err != nil {
			t.Fatalf("LoadState(watching=%v): %v", watching, err)
		}
		got := reloaded.Apps["203"].Watching
		if got != watching {
			t.Fatalf("Watching round-trip = %v, want %v", got, watching)
		}
	}
}

// IsWatched resolves by ID first, then by name, and fails open for apps it
// cannot resolve (the `phelix remove` tombstone case — see watching.go).
func TestIsWatched_ResolvesByIDAndName(t *testing.T) {
	isolateStateFile(t)

	// Persist through the real state file: ListApplications reloads from disk
	// on every call, so in-memory-only seeding would be wiped.
	m := &AppManager{Apps: map[string]*AppInfo{
		"a": {ID: "a", Name: "watched", Status: "stopped", Watching: true},
		"b": {ID: "b", Name: "unwatched", Status: "stopped"},
	}}
	if err := m.SaveState(); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	orig := Manager
	Manager = m
	t.Cleanup(func() { Manager = orig })

	if !IsWatched("a", "") {
		t.Fatal("IsWatched by ID: watched app must be true")
	}
	if IsWatched("", "unwatched") {
		t.Fatal("IsWatched by name: unwatched app must be false")
	}
	if IsWatched("b", "watched") {
		t.Fatal("ID must take precedence over a mismatched name")
	}
	// Unresolvable: fail open so lifecycle tombstones still reach the backend.
	if !IsWatched("gone", "") {
		t.Fatal("IsWatched for an unresolvable app must fail open (true)")
	}
}

// ListApplications and StatusApplication must carry the watching state into
// the read models the reporting boundary and the CLI tables consume.
func TestReadModelsCarryWatching(t *testing.T) {
	isolateStateFile(t)

	m := &AppManager{Apps: map[string]*AppInfo{
		"a": {ID: "a", Name: "watched", Status: "stopped", Watching: true},
		"b": {ID: "b", Name: "unwatched", Status: "stopped"},
	}}
	if err := m.SaveState(); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var watched, unwatched *AppListItem
	for _, item := range m.ListApplications() {
		switch item.Name {
		case "watched":
			watched = &item
		case "unwatched":
			unwatched = &item
		}
	}
	if watched == nil || unwatched == nil {
		t.Fatalf("expected both apps in list, got %+v", m.ListApplications())
	}
	if !watched.Watching || unwatched.Watching {
		t.Fatalf("ListApplications Watching: watched=%v unwatched=%v", watched.Watching, unwatched.Watching)
	}

	st, err := m.StatusApplication("watched")
	if err != nil {
		t.Fatalf("StatusApplication: %v", err)
	}
	if !st.Watching {
		t.Fatal("AppStatus.Watching must be true for the watched app")
	}
}

// AnyWatched is the gate for server-level reporting (server identity, server
// metrics, self logs, agent metadata): the server transmits its own data only
// while at least one registered app is watched.
func TestAnyWatched(t *testing.T) {
	isolateStateFile(t)

	seed := func(apps map[string]*AppInfo) {
		t.Helper()
		m := &AppManager{Apps: apps}
		if err := m.SaveState(); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		orig := Manager
		Manager = m
		t.Cleanup(func() { Manager = orig })
	}

	// No apps at all: nothing is transmitted about the server.
	seed(map[string]*AppInfo{})
	if AnyWatched() {
		t.Fatal("AnyWatched with no apps must be false")
	}

	// Only unwatched apps: still nothing.
	seed(map[string]*AppInfo{
		"a": {ID: "a", Name: "unwatched", Status: "stopped"},
		"b": {ID: "b", Name: "also-unwatched", Status: "stopped"},
	})
	if AnyWatched() {
		t.Fatal("AnyWatched with only unwatched apps must be false")
	}

	// One watched app among several: server data flows.
	seed(map[string]*AppInfo{
		"a": {ID: "a", Name: "unwatched", Status: "stopped"},
		"b": {ID: "b", Name: "watched", Status: "stopped", Watching: true},
	})
	if !AnyWatched() {
		t.Fatal("AnyWatched with one watched app must be true")
	}
}
