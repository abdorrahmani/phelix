package grpc

import (
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/health"
)

// healthAppStub (defined in health_snapshot_test.go) serves a fixed app list.

// setupHealthHandler isolates HOME, installs an app stub, and resets the
// process-global idempotency cache so request_ids never leak between tests.
func setupHealthHandler(t *testing.T, apps ...app.AppListItem) {
	t.Helper()
	setupTestSession(t)
	origMgr := app.Manager
	app.Manager = &healthAppStub{items: apps}
	healthResults = newHealthResultCache(healthResultCacheCapacity)
	t.Cleanup(func() {
		app.Manager = origMgr
		healthResults = newHealthResultCache(healthResultCacheCapacity)
	})
}

func testApp() app.AppListItem {
	return app.AppListItem{ID: "app-1", Name: "shop", Port: 3000}
}

func addCmd(reqID, name, url string) *pb.HealthCommand {
	return &pb.HealthCommand{
		RequestId: reqID,
		Command: &pb.HealthCommand_Add{Add: &pb.HealthAddCommand{
			AppId: "app-1", Name: name, Url: url,
		}},
	}
}

func mustConfig(t *testing.T, appID string) *health.AppHealthConfig {
	t.Helper()
	cm, err := health.InitConfigManager()
	if err != nil {
		t.Fatalf("InitConfigManager: %v", err)
	}
	return cm.GetConfig(appID)
}

func TestHealthAdd_ReplaySameRequestIsIdempotent(t *testing.T) {
	setupHealthHandler(t, testApp())

	r1 := ProcessHealthCommand(addCmd("req-1", "api", "http://localhost:3000/health"))
	if !r1.GetSuccess() {
		t.Fatalf("first add failed: %+v", r1)
	}
	// Redelivery of the SAME command replays the cached success — not ALREADY_EXISTS.
	r2 := ProcessHealthCommand(addCmd("req-1", "api", "http://localhost:3000/health"))
	if !r2.GetSuccess() {
		t.Fatalf("replayed add failed: code=%s err=%s", r2.GetErrorCode(), r2.GetError())
	}
	if cfg := mustConfig(t, "app-1"); len(cfg.Endpoints) != 1 {
		t.Fatalf("endpoint count = %d, want 1 (replay must not double-apply)", len(cfg.Endpoints))
	}
}

func TestHealthAdd_NaturalIdempotencyWithFreshRequestID(t *testing.T) {
	setupHealthHandler(t, testApp())

	if r := ProcessHealthCommand(addCmd("req-1", "api", "http://localhost:3000/health")); !r.GetSuccess() {
		t.Fatalf("first add failed: %+v", r)
	}
	// A different request_id (e.g. after a restart lost the cache) re-adding an
	// identical endpoint must still succeed, not fail with ALREADY_EXISTS.
	r := ProcessHealthCommand(addCmd("req-2", "api", "http://localhost:3000/health"))
	if !r.GetSuccess() {
		t.Fatalf("natural idempotency failed: code=%s err=%s", r.GetErrorCode(), r.GetError())
	}
}

func TestHealthAdd_SameRequestDifferentPayloadConflicts(t *testing.T) {
	setupHealthHandler(t, testApp())

	if r := ProcessHealthCommand(addCmd("req-1", "api", "http://a")); !r.GetSuccess() {
		t.Fatalf("first add failed: %+v", r)
	}
	r := ProcessHealthCommand(addCmd("req-1", "api", "http://DIFFERENT"))
	if r.GetSuccess() || r.GetErrorCode() != string(phelixerr.CodeIdempotencyConflict) {
		t.Fatalf("want IDEMPOTENCY_CONFLICT, got success=%v code=%s", r.GetSuccess(), r.GetErrorCode())
	}
}

func TestHealthRemove_ReplayAndAbsentAreIdempotent(t *testing.T) {
	setupHealthHandler(t, testApp())
	if r := ProcessHealthCommand(addCmd("a1", "api", "http://a")); !r.GetSuccess() {
		t.Fatalf("add failed: %+v", r)
	}
	rm := func(id string) *pb.HealthResult {
		return ProcessHealthCommand(&pb.HealthCommand{
			RequestId: id,
			Command:   &pb.HealthCommand_Remove{Remove: &pb.HealthRemoveCommand{AppId: "app-1", Name: "api"}},
		})
	}
	if r := rm("rm-1"); !r.GetSuccess() {
		t.Fatalf("remove failed: %+v", r)
	}
	// Removing an already-absent endpoint (fresh request_id) must succeed.
	if r := rm("rm-2"); !r.GetSuccess() {
		t.Fatalf("idempotent remove of absent endpoint failed: code=%s err=%s", r.GetErrorCode(), r.GetError())
	}
}

func TestHealthAdd_UnknownAppReturnsNotFoundCode(t *testing.T) {
	setupHealthHandler(t, testApp())
	r := ProcessHealthCommand(&pb.HealthCommand{
		RequestId: "x",
		Command:   &pb.HealthCommand_Add{Add: &pb.HealthAddCommand{AppId: "nope", Name: "api", Url: "http://a"}},
	})
	if r.GetSuccess() || r.GetErrorCode() != string(phelixerr.CodeNotFound) {
		t.Fatalf("want NOT_FOUND, got success=%v code=%s", r.GetSuccess(), r.GetErrorCode())
	}
}

func TestHealthSetEnabled_TogglesConfig(t *testing.T) {
	setupHealthHandler(t, testApp())
	if r := ProcessHealthCommand(addCmd("a1", "api", "http://a")); !r.GetSuccess() {
		t.Fatalf("add failed: %+v", r)
	}
	setEnabled := func(id string, enabled bool) *pb.HealthResult {
		return ProcessHealthCommand(&pb.HealthCommand{
			RequestId: id,
			Command: &pb.HealthCommand_SetEnabled{SetEnabled: &pb.HealthSetEnabledCommand{
				AppId: "app-1", Enabled: enabled,
			}},
		})
	}
	if r := setEnabled("e1", false); !r.GetSuccess() {
		t.Fatalf("disable failed: %+v", r)
	}
	if cfg := mustConfig(t, "app-1"); cfg.Enabled {
		t.Fatal("config should be disabled")
	}
	if r := setEnabled("e2", true); !r.GetSuccess() {
		t.Fatalf("enable failed: %+v", r)
	}
	if cfg := mustConfig(t, "app-1"); !cfg.Enabled {
		t.Fatal("config should be enabled")
	}
}

func TestHealthSetEnabled_NoConfigIsNotFound(t *testing.T) {
	setupHealthHandler(t, testApp())
	r := ProcessHealthCommand(&pb.HealthCommand{
		RequestId: "e1",
		Command:   &pb.HealthCommand_SetEnabled{SetEnabled: &pb.HealthSetEnabledCommand{AppId: "app-1", Enabled: false}},
	})
	if r.GetSuccess() || r.GetErrorCode() != string(phelixerr.CodeNotFound) {
		t.Fatalf("want NOT_FOUND, got success=%v code=%s", r.GetSuccess(), r.GetErrorCode())
	}
}

func TestHealthUpdate_PreservesEndpointIDAcrossRename(t *testing.T) {
	setupHealthHandler(t, testApp())
	if r := ProcessHealthCommand(addCmd("a1", "api", "http://localhost:3000/health")); !r.GetSuccess() {
		t.Fatalf("add failed: %+v", r)
	}
	id := mustConfig(t, "app-1").Endpoints["api"].ID
	if id == "" {
		t.Fatal("endpoint id was not minted")
	}

	r := ProcessHealthCommand(&pb.HealthCommand{
		RequestId: "u1",
		Command: &pb.HealthCommand_Update{Update: &pb.HealthUpdateCommand{
			AppId: "app-1", EndpointId: id, Name: "api-v2", Url: "http://localhost:3000/healthz",
		}},
	})
	if !r.GetSuccess() {
		t.Fatalf("update failed: code=%s err=%s", r.GetErrorCode(), r.GetError())
	}
	cfg := mustConfig(t, "app-1")
	if _, gone := cfg.Endpoints["api"]; gone {
		t.Fatal("old endpoint name should be gone after rename")
	}
	ep, ok := cfg.Endpoints["api-v2"]
	if !ok {
		t.Fatal("renamed endpoint missing")
	}
	if ep.ID != id {
		t.Fatalf("endpoint id changed on rename: %s -> %s (identity/history would be lost)", id, ep.ID)
	}
	if ep.URL != "http://localhost:3000/healthz" {
		t.Fatalf("url not updated: %s", ep.URL)
	}
}

func TestHealthSet_InvalidIntervalRejected(t *testing.T) {
	setupHealthHandler(t, testApp())
	r := ProcessHealthCommand(&pb.HealthCommand{
		RequestId: "s1",
		Command: &pb.HealthCommand_Set{Set: &pb.HealthSetCommand{
			AppId: "app-1", Path: "/health", Interval: "not-a-duration",
		}},
	})
	if r.GetSuccess() || r.GetErrorCode() != string(phelixerr.CodeInvalidArgument) {
		t.Fatalf("want INVALID_ARGUMENT, got success=%v code=%s", r.GetSuccess(), r.GetErrorCode())
	}
}

func TestHealthSet_PersistsDeployTierParams(t *testing.T) {
	setupHealthHandler(t, testApp())
	r := ProcessHealthCommand(&pb.HealthCommand{
		RequestId: "s1",
		Command: &pb.HealthCommand_Set{Set: &pb.HealthSetCommand{
			AppId: "app-1", Path: "/health", Mode: "http",
			DeployInterval: "2s", DeployRetries: 7, DeployTimeout: "45s",
		}},
	})
	if !r.GetSuccess() {
		t.Fatalf("set failed: %+v", r)
	}
	dt := mustConfig(t, "app-1").DeployTier
	if dt == nil || dt.Interval != "2s" || dt.Retries != 7 || dt.Timeout != "45s" || string(dt.Mode) != "http" {
		t.Fatalf("deploy tier not persisted: %+v", dt)
	}
}

// TestRemoteMutation_ReflectedInSnapshot ties a remote mutation to the resulting
// AppHealthSnapshot: disable flips enabled while keeping endpoints, and removing
// the last endpoint yields an empty-list snapshot (the backend's delete-all).
func TestRemoteMutation_ReflectedInSnapshot(t *testing.T) {
	setupHealthHandler(t, testApp())
	d, err := health.InitDaemon()
	if err != nil {
		t.Fatalf("InitDaemon: %v", err)
	}

	if r := ProcessHealthCommand(addCmd("a1", "api", "http://a")); !r.GetSuccess() {
		t.Fatalf("add api: %+v", r)
	}
	if r := ProcessHealthCommand(addCmd("a2", "web", "http://b")); !r.GetSuccess() {
		t.Fatalf("add web: %+v", r)
	}

	setEnabled := func(id string, on bool) {
		r := ProcessHealthCommand(&pb.HealthCommand{
			RequestId: id,
			Command:   &pb.HealthCommand_SetEnabled{SetEnabled: &pb.HealthSetEnabledCommand{AppId: "app-1", Enabled: on}},
		})
		if !r.GetSuccess() {
			t.Fatalf("set_enabled(%v): %+v", on, r)
		}
	}

	setEnabled("e1", false)
	snap := d.SnapshotForApp("app-1", "shop")
	if snap == nil {
		t.Fatal("snapshot nil though config exists")
	}
	if snap.Enabled {
		t.Fatal("snapshot should report disabled")
	}
	if len(snap.Endpoints) != 2 {
		t.Fatalf("endpoints = %d, want 2 (disable keeps endpoints)", len(snap.Endpoints))
	}

	setEnabled("e2", true)
	for _, ep := range []struct{ id, name string }{{"r1", "api"}, {"r2", "web"}} {
		if r := ProcessHealthCommand(&pb.HealthCommand{
			RequestId: ep.id,
			Command:   &pb.HealthCommand_Remove{Remove: &pb.HealthRemoveCommand{AppId: "app-1", Name: ep.name}},
		}); !r.GetSuccess() {
			t.Fatalf("remove %s: %+v", ep.name, r)
		}
	}
	snap = d.SnapshotForApp("app-1", "shop")
	if snap == nil {
		t.Fatal("snapshot nil; a config with zero endpoints must still snapshot (delete-all)")
	}
	if len(snap.Endpoints) != 0 {
		t.Fatalf("endpoints = %d, want 0 (empty-list delete-all)", len(snap.Endpoints))
	}
}
