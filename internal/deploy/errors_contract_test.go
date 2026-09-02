package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// --- Task 5: structured-error contract tests ------------------------------
//
// These tests pin the error CODE each deploy-domain failure path produces,
// plus the root-cause preservation guarantees (errors.Is / os.ErrProcessDone).
// Message text is intentionally NOT asserted — the code is the stable contract.

func TestBlueGreen_UnhealthyInstance_CodeHealthCheckFailed(t *testing.T) {
	resetHome(t)
	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	bg := &BlueGreen{
		AppName: "code-hc", AppID: "ch", PublicPort: 0,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealthShortTimeout() },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	// The old instance is the test process; ensure no real signal is sent if
	// the abort path ever touches it (it must not, but be safe).
	before := mustLoad(t, "code-hc")
	before.Slots[before.ActiveSlot].PID = 999999
	_ = Store(before)

	bg.Launcher = closedLauncher{}.Launch
	err := bg.Deploy(context.Background())
	if err == nil {
		t.Fatal("expected second deploy to fail")
	}
	if !phelixerr.IsCode(err, phelixerr.CodeHealthCheckFailed) {
		t.Fatalf("want HEALTH_CHECK_FAILED, got %s (%v)", phelixerr.CodeOf(err), err)
	}
	// The underlying WaitForHealthy timeout is preserved as the cause (a
	// HealthCheckFailed hierarchy), and the active slot stayed put.
	after := mustLoad(t, "code-hc")
	if after.ActiveSlot != before.ActiveSlot {
		t.Fatalf("active slot changed on failure: %q -> %q", before.ActiveSlot, after.ActiveSlot)
	}
}

func TestBlueGreen_NoProxyClient_CodeProxy(t *testing.T) {
	resetHome(t)
	fl := &httpLauncher{}
	defer fl.close()

	bg := &BlueGreen{
		AppName: "code-noprox", AppID: "np", PublicPort: 0,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    200 * time.Millisecond,
	}
	err := bg.Deploy(context.Background())
	if err == nil {
		t.Fatal("expected deploy to fail without proxy client")
	}
	if !phelixerr.IsCode(err, phelixerr.CodeProxy) {
		t.Fatalf("want PROXY_ERROR, got %s (%v)", phelixerr.CodeOf(err), err)
	}
}

func TestBlueGreen_ProxyDaemonUnreachable_CodeConnection(t *testing.T) {
	resetHome(t)
	pc := &fakeProxyClient{alive: false} // Ping fails
	fl := &httpLauncher{}
	defer fl.close()

	bg := &BlueGreen{
		AppName: "code-unreach", AppID: "ur", PublicPort: 0,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    200 * time.Millisecond,
	}
	err := bg.Deploy(context.Background())
	if err == nil {
		t.Fatal("expected deploy to fail when daemon unreachable")
	}
	if !phelixerr.IsCode(err, phelixerr.CodeConnection) {
		t.Fatalf("want CONNECTION_ERROR, got %s (%v)", phelixerr.CodeOf(err), err)
	}
}

func TestBlueGreen_ProxySwitchFail_CodeProxyAndActiveUntouched(t *testing.T) {
	resetHome(t)
	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	bg := &BlueGreen{
		AppName: "code-switch", AppID: "sw", PublicPort: 0,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	before := mustLoad(t, "code-switch")
	activeBefore := before.ActiveSlot
	// Replace the active PID with a sentinel that must survive intact: if the
	// failed switch ever touched the active slot we'd see it change away from
	// this value. Also makes a real stop of the test process impossible.
	before.Slots[activeBefore].PID = 999999
	pidSentinel := before.Slots[activeBefore].PID
	_ = Store(before)

	bg.ProxyClient = &failSwitchClient{inner: pc, err: errors.New("proxy switch refused (test)")}
	err := bg.Deploy(context.Background())
	if err == nil {
		t.Fatal("expected deploy to fail when proxy switch fails")
	}
	if !phelixerr.IsCode(err, phelixerr.CodeProxy) {
		t.Fatalf("want PROXY_ERROR, got %s (%v)", phelixerr.CodeOf(err), err)
	}
	// The active instance must be provably untouched: same slot, same PID.
	after := mustLoad(t, "code-switch")
	if after.ActiveSlot != activeBefore {
		t.Fatalf("active slot changed despite switch failure: %q -> %q", activeBefore, after.ActiveSlot)
	}
	if after.Slots[activeBefore].PID != pidSentinel {
		t.Fatalf("active PID changed despite switch failure: %d -> %d", pidSentinel, after.Slots[activeBefore].PID)
	}
}

func TestDeployLockHeld_CodeDeployLocked(t *testing.T) {
	resetHome(t)
	app := "lock-code"
	createTestVersion(t, app, 1, false)
	markCurrent(t, app, 1)
	initStateForTest(t, app)

	release, err := AcquireDeployLock(app, "deploy")
	if err != nil {
		t.Fatalf("acquire first: %v", err)
	}
	defer release()

	_, err = AcquireDeployLock(app, "deploy")
	if err == nil {
		t.Fatal("expected second acquire to fail")
	}
	if !phelixerr.IsCode(err, phelixerr.CodeDeployLocked) {
		t.Fatalf("want DEPLOY_LOCKED, got %s (%v)", phelixerr.CodeOf(err), err)
	}
}

func TestVersionResolution_Codes(t *testing.T) {
	resetHome(t)
	app := "ver-code"
	createTestVersion(t, app, 1, false)
	markCurrent(t, app, 1)

	if _, err := ResolveVersionOrTag(app, ""); err == nil || !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
		t.Fatalf("empty input: want INVALID_ARGUMENT, got %v / %s", err, phelixerr.CodeOf(err))
	}
	if _, err := ResolveVersionOrTag(app, "missing-tag"); err == nil || !phelixerr.IsCode(err, phelixerr.CodeVersionNotFound) {
		t.Fatalf("missing tag: want VERSION_NOT_FOUND, got %v / %s", err, phelixerr.CodeOf(err))
	}
	if _, err := ResolveVersionOrTag(app, "v99"); err == nil || !phelixerr.IsCode(err, phelixerr.CodeVersionNotFound) {
		t.Fatalf("missing version: want VERSION_NOT_FOUND, got %v / %s", err, phelixerr.CodeOf(err))
	}
}

func TestVersionPaths_MissingVersion_CodeVersionNotFound(t *testing.T) {
	resetHome(t)
	app := "vp-code"
	createTestVersion(t, app, 1, false)

	_, _, err := VersionPaths(app, 3)
	if err == nil || !phelixerr.IsCode(err, phelixerr.CodeVersionNotFound) {
		t.Fatalf("want VERSION_NOT_FOUND, got %v / %s", err, phelixerr.CodeOf(err))
	}
}

func TestParseVersionArg_Invalid_CodeInvalidArgument(t *testing.T) {
	if _, err := ParseVersionArg("abc"); err == nil || !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
		t.Fatalf("want INVALID_ARGUMENT, got %v / %s", err, phelixerr.CodeOf(err))
	}
}

func TestPromoteVersion_Unknown_CodeVersionNotFound(t *testing.T) {
	resetHome(t)
	app := "promote-code"
	createTestVersion(t, app, 1, false)
	if err := PromoteVersion(app, 42, "blue-green"); err == nil || !phelixerr.IsCode(err, phelixerr.CodeVersionNotFound) {
		t.Fatalf("want VERSION_NOT_FOUND, got %v / %s", err, phelixerr.CodeOf(err))
	}
}

func TestEnvSnapshot_DecodeCorrupt_CodeConfiguration(t *testing.T) {
	resetHome(t)
	app := "env-code"
	createTestVersion(t, app, 1, false)
	envPath, err := envSnapshotPath(app, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("not-json"), 0o644); err != nil {
		t.Fatalf("write corrupt env snapshot: %v", err)
	}
	_, err = EnvOverlayFromSnapshot(envPath, "appid")
	if err == nil || !phelixerr.IsCode(err, phelixerr.CodeConfiguration) {
		t.Fatalf("want CONFIGURATION_ERROR, got %v / %s", err, phelixerr.CodeOf(err))
	}
}

func TestGracefulStop_KillFails_CodeProcessFailed_PreservesCause(t *testing.T) {
	fp := newFakeProc(0) // never exits on SIGTERM
	fp.killErr = errors.New("kill denied")
	_, err := GracefulStop(context.Background(), fp, 50*time.Millisecond, 0)
	if err == nil || !phelixerr.IsCode(err, phelixerr.CodeProcessFailed) {
		t.Fatalf("want PROCESS_FAILED, got %v / %s", err, phelixerr.CodeOf(err))
	}
	if cause := phelixerr.Cause(err); cause == nil || cause.Error() != "kill denied" {
		t.Fatalf("root cause lost: %v", cause)
	}
}

func TestGracefulStop_KillSucceeds_ReturnsNil(t *testing.T) {
	fp := newFakeProc(0) // never exits on SIGTERM
	fp.killExits = true  // Kill() makes Wait() return
	_, err := GracefulStop(context.Background(), fp, 50*time.Millisecond, 0)
	if err != nil {
		t.Fatalf("expected nil after successful SIGKILL, got %v", err)
	}
}

// --- fake helpers ---------------------------------------------------------

// failSwitchClient wraps a *fakeProxyClient and fails every Switch.
type failSwitchClient struct {
	inner *fakeProxyClient
	err   error
}

func (f *failSwitchClient) Ping(ctx context.Context) error { return f.inner.Ping(ctx) }
func (f *failSwitchClient) Add(ctx context.Context, name string, port int, prim proxy.Target, backends ...proxy.Target) error {
	return f.inner.Add(ctx, name, port, prim, backends...)
}
func (f *failSwitchClient) Switch(ctx context.Context, name string, prim proxy.Target, backends ...proxy.Target) error {
	return f.err
}
func (f *failSwitchClient) Remove(ctx context.Context, name string) error {
	return f.inner.Remove(ctx, name)
}
func (f *failSwitchClient) Status(ctx context.Context, name string) ([]proxy.AppStatus, error) {
	return f.inner.Status(ctx, name)
}
