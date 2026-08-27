package deploy

import (
	"context"
	"os"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
)

// Regression coverage for the zero-downtime audit fixes (blue-green side).
// Shared fakes (fakeLogger, fakeProxyClient, stubBuilder, selfProc,
// multiLauncher in rolling file, resetHome, mustLoad) live in the companion
// *_test.go files of this package.

// countingProc counts termination attempts so tests can prove an instance was
// shut down instead of leaked.
type countingProc struct {
	selfProc
	signals int64 // accessed from the single Deploy goroutine only
}

func (c *countingProc) Signal(os.Signal) error {
	c.signals++
	return nil
}
func (c *countingProc) Kill() error { return nil }

// fastCfg mirrors fastHealth but named distinctly to avoid future collisions.
func fastCfg() *health.DeployTierConfig {
	return &health.DeployTierConfig{Interval: "5ms", Retries: 2, Timeout: "3s"}
}

// Bug regression: aborting at the switch stage with a nil ProxyClient used to
// return early and orphan a healthy running instance forever.
func TestBlueGreen_NilClientAtSwitch_KillsInstanceNotLeak(t *testing.T) {
	resetHome(t)

	fl := &multiLauncher{}
	defer fl.close()
	pc := &fakeProxyClient{alive: true}

	bg := &BlueGreen{
		AppName: "leak-app", AppID: "la", PublicPort: 8099,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastCfg() },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	bg.ProxyClient = nil // proxy disappears mid-deploy
	if err := bg.Deploy(context.Background()); err == nil {
		t.Fatal("expected second deploy to fail without proxy client")
	} else if got := phelixerr.CodeOf(err); got != phelixerr.CodeProxy {
		t.Fatalf("want PROXY_ERROR, got %s (%v)", got, err)
	}

	last := fl.lastProc()
	if last == nil || last.signals == 0 {
		t.Fatalf("replacement instance leaked: termination attempted %d times", lastSignals(last))
	}
	st := mustLoad(t, "leak-app")
	inactive := st.InactiveSlot()
	if st.Slots[inactive].Status != "failed" || st.Slots[inactive].PID != 0 {
		t.Fatalf("slot not cleaned after switch abort: %+v", st.Slots[inactive])
	}
}

func lastSignals(p *countingProc) int64 {
	if p == nil {
		return 0
	}
	return p.signals
}

// Bug regression: a crashed deploy used to leave the non-active slot claiming
// a live process whose record was then silently overwritten next deploy,
// orphaning it. The recovery pass must clear/redeploy that slot explicitly.
func TestBlueGreen_StaleInactiveSlotReclaimed(t *testing.T) {
	resetHome(t)

	fl := &multiLauncher{}
	defer fl.close()
	pc := &fakeProxyClient{alive: true}
	bg := &BlueGreen{
		AppName: "stale-app", AppID: "sa", PublicPort: 8101,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastCfg() },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	before := mustLoad(t, "stale-app")
	inactive := before.InactiveSlot()
	before.Slots[inactive] = &Instance{
		Slot: inactive, PID: os.Getpid(), Port: 1,
		BinaryPath: "/bin/true", Status: "running",
	}
	if err := Store(before); err != nil {
		t.Fatal(err)
	}

	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("second deploy: %v", err)
	}
	got := mustLoad(t, "stale-app").Slots[inactive]
	if got.Status != "running" || got.PID <= 0 || got.Port <= 1 {
		t.Fatalf("inactive slot not redeployed over stale record: %+v", got)
	}
}

// PID-recycling guard: findVerifiedProcess must refuse signalled handles when
// the recorded executable does not match the live process.
func TestFindVerifiedProcess_IdentityGate(t *testing.T) {
	self := os.Getpid()

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve executable: %v", err)
	}
	if findVerifiedProcess(self, exe) == nil {
		t.Fatalf("expected verified handle for own executable %s", exe)
	}
	if p := findVerifiedProcess(self, "/definitely/not/me"); p != nil {
		t.Fatalf("mismatched identity yielded handle %+v", p)
	}
	if p := findVerifiedProcess(999999, ""); p != nil {
		t.Fatalf("dead pid yielded handle %+v", p)
	}
	if p := findVerifiedProcess(self, ""); p == nil {
		t.Fatal("legacy empty-expectation lookup refused a live pid")
	}
}

// Lock: TryLoadLock reports holder while held, nil once released (and must
// never fabricate a bare deploy.json — that broke mode detection downstream).
func TestAcquireDeployLock_TryLoadAndNoBareState(t *testing.T) {
	resetHome(t)
	app := "lock-audit"

	lock, err := TryLoadLock(app)
	if err != nil || lock != nil {
		t.Fatalf("fresh app should be unlocked (got %+v, %v)", lock, err)
	}

	release, err := AcquireDeployLock(app, "probe")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	holder, err := TryLoadLock(app)
	if err != nil {
		t.Fatal(err)
	}
	if holder == nil || holder.Operation != "probe" || holder.PID != os.Getpid() {
		t.Fatalf("holder mismatch: %+v", holder)
	}
	release()

	if free, ferr := TryLoadLock(app); free != nil || ferr != nil {
		t.Fatalf("lock not freed (%+v, %v)", free, ferr)
	}

	release2, err := AcquireDeployLock(app+"2", "probe2")
	if err != nil {
		t.Fatalf("acquire#2: %v", err)
	}
	defer release2()
	if _, lerr := Load(app + "2"); !os.IsNotExist(lerr) {
		t.Fatalf("lock acquisition fabricated deploy.json: %v", lerr)
	}
}
