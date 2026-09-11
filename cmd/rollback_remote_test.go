package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/monitor"
)

// Tests for the remote (MonitorStream) rollback command handler. The remote
// path must behave exactly like the local CLI rollback: same target
// resolution, same dry-run safety, same telemetry. Reuses the
// seedRollbackPreviewApp harness from rollback_dryrun_test.go.

// withRemoteManager replaces app.Manager with a real AppManager carrying one
// seeded app — GetAppInfo type-asserts the concrete type, so a stub interface
// would panic. RemoteRollback reads the daemon's in-memory registry (no
// LoadState), like every other remote command.
func withRemoteManager(t *testing.T) {
	t.Helper()
	orig := app.Manager
	app.Manager = &app.AppManager{Apps: map[string]*app.AppInfo{
		"app-uuid-1": {ID: "app-uuid-1", Name: "shop", Port: 8080, Status: "running"},
	}}
	t.Cleanup(func() { app.Manager = orig })
}

// snapshotAppDir records every file state the rollback engine can touch so
// tests can assert "nothing mutated".
func snapshotAppDir(t *testing.T, home, name string) map[string]string {
	t.Helper()
	appDir := filepath.Join(home, ".phelix", "apps", name)
	out := map[string]string{}
	entries, err := os.ReadDir(appDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := filepath.Join(appDir, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			link, _ := os.Readlink(p)
			out[p] = "symlink→" + link
			continue
		}
		if e.IsDir() {
			continue
		}
		data, _ := os.ReadFile(p)
		out[p] = string(data)
	}
	return out
}

func TestRemoteRollback_MissingApp(t *testing.T) {
	withRemoteManager(t)
	t.Setenv("HOME", t.TempDir())

	err := RemoteRollback(monitor.CommandPayload{AppName: "ghost", Target: "v2"})
	if !phelixerr.IsCode(err, phelixerr.CodeNotFound) {
		t.Fatalf("expected NOT_FOUND, got %v", err)
	}
}

func TestRemoteRollback_MissingAppNameRejected(t *testing.T) {
	withRemoteManager(t)
	t.Setenv("HOME", t.TempDir())

	err := RemoteRollback(monitor.CommandPayload{Target: "v2"})
	if !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
		t.Fatalf("expected INVALID_ARGUMENT for empty app, got %v", err)
	}
}

func TestRemoteRollback_InvalidTarget(t *testing.T) {
	withRemoteManager(t)
	seedRollbackPreviewApp(t, "shop", deploy.ModeBlueGreen)

	err := RemoteRollback(monitor.CommandPayload{AppName: "shop", Target: "v99"})
	if !phelixerr.IsCode(err, phelixerr.CodeRollbackTargetNotFound) {
		t.Fatalf("expected ROLLBACK_TARGET_NOT_FOUND, got %v", err)
	}
}

func TestRemoteRollback_TagTarget(t *testing.T) {
	withRemoteManager(t)
	seedRollbackPreviewApp(t, "shop", deploy.ModeBlueGreen)

	// Dry-run so the test never executes a real rollback; resolution still
	// goes through the full shared path.
	if err := RemoteRollback(monitor.CommandPayload{AppName: "shop", Target: "stable", DryRun: true}); err != nil {
		t.Fatalf("tag resolution failed: %v", err)
	}
}

func TestRemoteRollback_NumericTarget(t *testing.T) {
	withRemoteManager(t)
	seedRollbackPreviewApp(t, "shop", deploy.ModeBlueGreen)

	if err := RemoteRollback(monitor.CommandPayload{AppName: "shop", Target: "2", DryRun: true}); err != nil {
		t.Fatalf("numeric resolution failed: %v", err)
	}
}

func TestRemoteRollback_PreviousVersionWhenNoTarget(t *testing.T) {
	withRemoteManager(t)
	seedRollbackPreviewApp(t, "shop", deploy.ModeBlueGreen)

	// No target = previous version (v2 when current is v3).
	if err := RemoteRollback(monitor.CommandPayload{AppName: "shop", DryRun: true}); err != nil {
		t.Fatalf("previous-version resolution failed: %v", err)
	}
}

func TestRemoteRollback_TargetIsCurrentRejected(t *testing.T) {
	withRemoteManager(t)
	seedRollbackPreviewApp(t, "shop", deploy.ModeBlueGreen)

	// v3 is IsCurrent in the seeded versions.json.
	err := RemoteRollback(monitor.CommandPayload{AppName: "shop", Target: "v3", DryRun: true})
	if !phelixerr.IsCode(err, phelixerr.CodeRollbackTargetNotFound) {
		t.Fatalf("expected ROLLBACK_TARGET_NOT_FOUND for rollback-to-current, got %v", err)
	}
}

func TestRemoteRollback_NegativeVerifyDuration(t *testing.T) {
	withRemoteManager(t)
	seedRollbackPreviewApp(t, "shop", deploy.ModeBlueGreen)

	err := RemoteRollback(monitor.CommandPayload{AppName: "shop", Target: "v2", VerifyDuration: -1000})
	if !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
		t.Fatalf("expected INVALID_ARGUMENT, got %v", err)
	}
}

func TestRemoteRollback_ReasonTooLong(t *testing.T) {
	withRemoteManager(t)
	seedRollbackPreviewApp(t, "shop", deploy.ModeBlueGreen)

	err := RemoteRollback(monitor.CommandPayload{AppName: "shop", Target: "v2", Reason: strings.Repeat("x", 501)})
	if !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
		t.Fatalf("expected INVALID_ARGUMENT for >500 char reason, got %v", err)
	}
}

// TestRemoteRollback_DryRunNoMutation pins the remote dry-run safety
// guarantee: nothing on disk changes and no lock is created.
func TestRemoteRollback_DryRunNoMutation(t *testing.T) {
	withRemoteManager(t)
	home := seedRollbackPreviewApp(t, "shop", deploy.ModeBlueGreen)
	appDir := filepath.Join(home, ".phelix", "apps", "shop")
	logPath := filepath.Join(appDir, "rollback.log")
	if err := os.WriteFile(logPath, []byte("preexisting\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := snapshotAppDir(t, home, "shop")
	if err := RemoteRollback(monitor.CommandPayload{AppName: "shop", Target: "v2", DryRun: true}); err != nil {
		t.Fatalf("dry-run failed: %v", err)
	}
	after := snapshotAppDir(t, home, "shop")

	for p, v := range before {
		if after[p] != v {
			t.Errorf("dry-run mutated %s", p)
		}
	}
	if _, err := os.Stat(filepath.Join(appDir, "deploy.lock")); err == nil {
		t.Error("dry-run created deploy.lock")
	}
}

func TestRemoteRollback_DryRunRequestID(t *testing.T) {
	withRemoteManager(t)
	seedRollbackPreviewApp(t, "shop", deploy.ModeBlueGreen)

	if err := RemoteRollback(monitor.CommandPayload{
		AppName: "shop", Target: "v2", DryRun: true, RequestID: "req-preview-7",
	}); err != nil {
		t.Fatalf("dry-run failed: %v", err)
	}
	// The preview path shares the same reporter stamping helper as execution;
	// assert that helper directly without depending on the async sender.
	plan, err := deploy.PlanRollback("shop", deploy.RollbackPlanInput{AppID: "app-uuid-1", Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	r := phelixgrpc.NewRollbackReporter("", "app-uuid-1", "app-uuid-1", "shop", plan.Strategy, plan.Strategy, "v3", "v2", "stable")
	r.SetRequestID("req-preview-7")
	if got := r.Event().GetRequestId(); got != "req-preview-7" {
		t.Fatalf("typed request_id=%q, want req-preview-7", got)
	}
	if got := r.Event().GetMetadata()["request_id"]; got != "req-preview-7" {
		t.Fatalf("request_id=%q, want req-preview-7", got)
	}
}

// TestRemoteRollback_RequestIDMetadataMapping pins the correlation contract:
// a non-empty payload RequestID becomes metadata["request_id"] on the
// rollback event stream; an empty one (local CLI) adds nothing. Asserted
// against the real RollbackReporter the executors use.
func TestRemoteRollback_RequestIDMetadataMapping(t *testing.T) {
	r := newRemoteTestReporter("req-remote-1")
	if got := r.Event().GetRequestId(); got != "req-remote-1" {
		t.Fatalf("typed request_id = %q, want req-remote-1", got)
	}
	if got := r.Event().GetMetadata()["request_id"]; got != "req-remote-1" {
		t.Fatalf("request_id metadata = %q, want req-remote-1", got)
	}

	local := newRemoteTestReporter("")
	if got := local.Event().GetMetadata()["request_id"]; got != "" {
		t.Fatalf("local rollback must not carry request_id metadata, got %q", got)
	}
}

// newRemoteTestReporter builds a real RollbackReporter and applies the same
// request_id stamping rollbackClassic/rollbackZeroDowntime perform.
func newRemoteTestReporter(requestID string) *phelixgrpc.RollbackReporter {
	r := phelixgrpc.NewRollbackReporter("", "app-uuid-1", "app-uuid-1", "shop", "blue-green", "blue-green", "v3", "v2", "stable")
	if requestID != "" {
		r.SetRequestID(requestID)
	}
	return r
}
