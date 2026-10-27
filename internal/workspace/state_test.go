package workspace

import (
	"strings"
	"testing"
)

func workspaceForStateTests() *Workspace {
	return &Workspace{
		Name:   "mesh",
		Root:   "/tmp/mesh",
		Source: SourceManifest,
		Services: []*Service{
			{Name: "auth", Path: "/tmp/mesh/auth", Port: 8081, Strategy: StrategyBlueGreen},
			{Name: "orders", Path: "/tmp/mesh/orders", DependsOn: []string{"auth"}},
		},
	}
}

func TestState_SaveLoadAndRecord(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	w := workspaceForStateTests()
	state := &State{}
	state.SyncMembership(w)
	if err := SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded, err := LoadState("mesh")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded == nil || len(loaded.Services) != 2 || loaded.Source != SourceManifest {
		t.Fatalf("loaded = %+v", loaded)
	}

	loaded.RecordServiceResult("auth", "deploy", "succeeded", 7, "")
	if err := SaveState(loaded); err != nil {
		t.Fatalf("SaveState after record: %v", err)
	}
	reloaded, err := LoadState("mesh")
	if err != nil {
		t.Fatalf("LoadState after record: %v", err)
	}
	auth := reloaded.Services[0]
	if auth.LastOperation != "deploy" || auth.LastResult != "succeeded" || auth.LastVersion != 7 {
		t.Fatalf("auth record = %+v", auth)
	}

	fresh := &State{}
	fresh.SyncMembership(w)
	if fresh.Services[0].LastOperation != "" {
		t.Fatalf("fresh membership must not carry old results: %+v", fresh.Services[0])
	}
}

func TestState_MissingIsNil(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	state, err := LoadState("never-orchestrated")
	if err != nil || state != nil {
		t.Fatalf("LoadState() = %v, %v; want nil, nil", state, err)
	}
}

func TestState_RecordUnknownServiceIsNoop(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	state := &State{}
	state.SyncMembership(workspaceForStateTests())
	state.RecordServiceResult("ghost", "build", "succeeded", 1, "")
	if state.Services[0].LastOperation != "" || state.Services[1].LastOperation != "" {
		t.Fatalf("recording an unknown service changed another entry: %+v", state.Services)
	}
}

func TestWorkspaceLock_ExcludesSecondHolder(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	release, err := AcquireWorkspaceLock("mesh", "deploy")
	if err != nil {
		t.Fatalf("AcquireWorkspaceLock: %v", err)
	}

	_, err = AcquireWorkspaceLock("mesh", "build")
	if err == nil {
		t.Fatalf("second AcquireWorkspaceLock should fail while held")
	}
	if !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("contention error = %v, want holder details", err)
	}

	release()
	second, err := AcquireWorkspaceLock("mesh", "build")
	if err != nil {
		t.Fatalf("AcquireWorkspaceLock after release: %v", err)
	}
	second()
}

func TestWorkspaceLock_NamespacesByName(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	releaseA, err := AcquireWorkspaceLock("mesh-a", "deploy")
	if err != nil {
		t.Fatalf("AcquireWorkspaceLock a: %v", err)
	}
	defer releaseA()
	releaseB, err := AcquireWorkspaceLock("mesh-b", "deploy")
	if err != nil {
		t.Fatalf("AcquireWorkspaceLock b: %v", err)
	}
	releaseB()
}
