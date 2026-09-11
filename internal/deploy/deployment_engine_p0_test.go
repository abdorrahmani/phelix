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

func TestVerifyRollbackStabilityZeroTargetsStructured(t *testing.T) {
	resetHome(t)
	app := "verify-empty-p0"
	if err := Store(&DeployState{AppName: app, Mode: ModeRolling, Replicas: map[string]*Instance{}}); err != nil {
		t.Fatal(err)
	}
	err := VerifyRollbackStability(context.Background(), VerificationOptions{AppName: app, Duration: time.Second})
	if err == nil || !phelixerr.IsCode(err, phelixerr.CodeHealthCheckFailed) {
		t.Fatalf("want HEALTH_CHECK_FAILED, got %v / %s", err, phelixerr.CodeOf(err))
	}
}

func TestExecuteRollbackPropagatesVersionsReadError(t *testing.T) {
	resetHome(t)
	app := "rollback-read-p0"
	initStateForTest(t, app)
	path, err := versionsPath(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	err = ExecuteRollback(context.Background(), RollbackOptions{AppName: app, TargetVersion: 1})
	if err == nil {
		t.Fatal("expected versions read error")
	}
	if phelixerr.IsCode(err, phelixerr.CodeRollbackTargetNotFound) {
		t.Fatalf("read error was collapsed to target-not-found: %v", err)
	}
}

func TestExecuteRollbackPreservesVersionNotFoundCode(t *testing.T) {
	resetHome(t)
	app := "rollback-code-p0"
	createTestVersion(t, app, 1, true)
	initStateForTest(t, app)
	err := ExecuteRollback(context.Background(), RollbackOptions{AppName: app, TargetVersion: 9})
	if err == nil || !phelixerr.IsCode(err, phelixerr.CodeVersionNotFound) {
		t.Fatalf("want VERSION_NOT_FOUND, got %v / %s", err, phelixerr.CodeOf(err))
	}
}

func TestExecuteRollbackRejectsChangedCurrentVersionUnderLock(t *testing.T) {
	resetHome(t)
	app := "rollback-revalidate-p0"
	createTestVersion(t, app, 1, false)
	createTestVersion(t, app, 2, true)
	initStateForTest(t, app)
	err := ExecuteRollback(context.Background(), RollbackOptions{
		AppName:                app,
		TargetVersion:          1,
		ExpectedCurrentVersion: 3,
	})
	if err == nil || !phelixerr.IsCode(err, phelixerr.CodeRollbackTargetNotFound) {
		t.Fatalf("want ROLLBACK_TARGET_NOT_FOUND, got %v / %s", err, phelixerr.CodeOf(err))
	}
}

func TestRollingRestoresCompleteOldRecordOnHealthFailure(t *testing.T) {
	resetHome(t)
	app := "rolling-restore-p0"
	old := &Instance{Slot: "0", PID: os.Getpid(), Port: 43210, BinaryPath: "/bin/true", EnvPath: "/old/env", Status: "running", Version: 7, StartedAt: time.Unix(12, 0)}
	state := &DeployState{AppName: app, Mode: ModeRolling, PublicPort: 8080, Replicas: map[string]*Instance{"0": cloneInstance(old)}}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	r := &Rolling{AppName: app, PublicPort: 8080, Replicas: 1, Builder: stubBuilder("/bin/true"), Launcher: closedLauncher{}.Launch, ProxyClient: &fakeProxyClient{alive: true}, Logger: &fakeLogger{}, HealthProvider: func(string) *health.DeployTierConfig { return fastHealthShortTimeout() }, GracePeriod: 10 * time.Millisecond}
	err := r.Deploy(context.Background())
	if err == nil || !phelixerr.IsCode(err, phelixerr.CodeHealthCheckFailed) {
		t.Fatalf("want HEALTH_CHECK_FAILED, got %v / %s", err, phelixerr.CodeOf(err))
	}
	got := mustLoad(t, app).Replicas["0"]
	if *got != *old {
		t.Fatalf("old record not fully restored:\n got %+v\nwant %+v", got, old)
	}
}

type compensatingProxy struct {
	calls [][]proxy.Target
}

func (p *compensatingProxy) Ping(context.Context) error { return nil }
func (p *compensatingProxy) Add(_ context.Context, _ string, _ int, primary proxy.Target, backends ...proxy.Target) error {
	p.calls = append(p.calls, append([]proxy.Target{primary}, backends...))
	return nil
}
func (p *compensatingProxy) Switch(_ context.Context, _ string, primary proxy.Target, backends ...proxy.Target) error {
	p.calls = append(p.calls, append([]proxy.Target{primary}, backends...))
	return nil
}
func (p *compensatingProxy) Remove(context.Context, string) error { return nil }
func (p *compensatingProxy) Status(context.Context, string) ([]proxy.AppStatus, error) {
	return []proxy.AppStatus{{AppName: "enrolled"}}, nil
}

func TestRollingPersistsBeforeDrainAndCompensates(t *testing.T) {
	resetHome(t)
	app := "rolling-compensate-p0"
	fl := &multiLauncher{}
	defer fl.close()
	_, oldPort, err := fl.Launch(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	old := &Instance{Slot: "0", PID: os.Getpid(), Port: oldPort, BinaryPath: "/bin/true", Status: "running", Version: 1}
	state := &DeployState{AppName: app, Mode: ModeRolling, PublicPort: 8080, Replicas: map[string]*Instance{"0": cloneInstance(old)}}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	pc := &compensatingProxy{}
	writes := 0
	r := &Rolling{AppName: app, PublicPort: 8080, Replicas: 1, Builder: stubBuilder("/bin/true"), Launcher: fl.Launch, ProxyClient: pc, Logger: &fakeLogger{}, HealthProvider: func(string) *health.DeployTierConfig { return fastCfg() }, GracePeriod: 10 * time.Millisecond}
	r.stateStore = func(s *DeployState) error {
		writes++
		if writes == 2 {
			return errors.New("disk full")
		}
		return Store(s)
	}
	err = r.Deploy(context.Background())
	if err == nil || !phelixerr.IsCode(err, phelixerr.CodeFilesystem) {
		t.Fatalf("want FILESYSTEM_ERROR, got %v / %s", err, phelixerr.CodeOf(err))
	}
	if len(pc.calls) != 2 || pc.calls[1][0].Host != hostPort(oldPort) {
		t.Fatalf("proxy was not compensated to old membership: %+v", pc.calls)
	}
	got := mustLoad(t, app).Replicas["0"]
	if got.Port != old.Port || got.Status != "running" {
		t.Fatalf("persisted state stopped describing old fleet: %+v", got)
	}
}

func TestPromoteVersionStagesBeforeMetadataCommit(t *testing.T) {
	resetHome(t)
	app := "promote-transaction-p0"
	createTestVersion(t, app, 1, true)
	createTestVersion(t, app, 2, false)
	dir, err := appDataDir(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := updateCurrentSymlink(app, 1); err != nil {
		t.Fatal(err)
	}
	// A directory at every possible staged-link parent operation makes symlink
	// creation fail before versions metadata is renamed.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	err = PromoteVersion(app, 2, "rolling")
	if os.Geteuid() == 0 {
		t.Skip("permission staging failure is not enforceable as root")
	}
	if err == nil {
		t.Fatal("expected staged promotion failure")
	}
	vf, loadErr := LoadVersions(app)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !vf.Versions[0].IsCurrent || vf.Versions[1].IsCurrent {
		t.Fatalf("metadata partially promoted: %+v", vf.Versions)
	}
	target, readErr := os.Readlink(filepath.Join(dir, "current"))
	if readErr != nil || target != filepath.Join("builds", "v1") {
		t.Fatalf("current changed: target=%q err=%v", target, readErr)
	}
}
