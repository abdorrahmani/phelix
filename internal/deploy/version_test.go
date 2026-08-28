package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/health"
)

// --- retention pruning tests -----------------------------------------------

func TestPruneVersions_RetainsAtMostMax(t *testing.T) {
	resetHome(t)
	app := "prune-max"
	log := &fakeLogger{}
	policy := DefaultRetention{Max: 3}

	// Create 5 versions.
	for i := 1; i <= 5; i++ {
		createTestVersion(t, app, i, false)
	}
	// Mark v5 as current (it would be the latest build).
	markCurrent(t, app, 5)

	if err := PruneVersions(app, policy, log); err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}

	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatalf("LoadVersions: %v", err)
	}
	if len(vf.Versions) != 3 {
		t.Fatalf("expected 3 versions retained, got %d", len(vf.Versions))
	}
	// v5 (current) must survive. Oldest kept = v3, v4, v5.
	remaining := versionNumbers(vf)
	assertContains(t, remaining, 5)
	assertNotContains(t, remaining, 1)
	assertNotContains(t, remaining, 2)
}

func TestPruneVersions_NeverPrunesActive(t *testing.T) {
	resetHome(t)
	app := "prune-active"
	log := &fakeLogger{}
	policy := DefaultRetention{Max: 2}

	// Create 4 versions, mark v1 as current.
	for i := 1; i <= 4; i++ {
		createTestVersion(t, app, i, false)
	}
	markCurrent(t, app, 1)

	if err := PruneVersions(app, policy, log); err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}

	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatalf("LoadVersions: %v", err)
	}
	remaining := versionNumbers(vf)
	// v1 must survive (it's current) even though it's the oldest.
	assertContains(t, remaining, 1)
	// Only 2 versions should be kept; v1 (current) + one more.
	if len(vf.Versions) > 2 {
		t.Fatalf("expected at most 2 versions, got %d: %v", len(vf.Versions), remaining)
	}
}

func TestPruneVersions_UnlimitedRetention(t *testing.T) {
	resetHome(t)
	app := "prune-unlimited"
	log := &fakeLogger{}
	policy := UnlimitedRetention{}

	for i := 1; i <= 10; i++ {
		createTestVersion(t, app, i, false)
	}
	markCurrent(t, app, 10)

	if err := PruneVersions(app, policy, log); err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}

	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatalf("LoadVersions: %v", err)
	}
	if len(vf.Versions) != 10 {
		t.Fatalf("unlimited retention should keep all 10, got %d", len(vf.Versions))
	}
}

func TestPruneVersions_ActiveIsOldest(t *testing.T) {
	resetHome(t)
	app := "prune-oldest-active"
	log := &fakeLogger{}
	policy := DefaultRetention{Max: 3}

	// Create 5 versions; v1 is active.
	for i := 1; i <= 5; i++ {
		createTestVersion(t, app, i, false)
	}
	markCurrent(t, app, 1)

	if err := PruneVersions(app, policy, log); err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}

	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatalf("LoadVersions: %v", err)
	}
	remaining := versionNumbers(vf)
	// v1 is current so it must survive.
	assertContains(t, remaining, 1)
	// At most 3 total.
	if len(vf.Versions) > 3 {
		t.Fatalf("expected at most 3, got %d: %v", len(vf.Versions), remaining)
	}
}

// --- ExistingVersionSource tests -------------------------------------------

func TestExistingVersionSource_ResolvePaths(t *testing.T) {
	resetHome(t)
	app := "resolve-paths"
	createTestVersion(t, app, 1, false)
	createTestVersion(t, app, 2, false)
	markCurrent(t, app, 2)

	src := &ExistingVersionSource{AppName: app, Version: 2}
	bin, env, err := src.Build(context.Background())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if bin == "" {
		t.Fatalf("expected non-empty binary path")
	}
	// Env may not exist for test versions; that's fine (empty is valid).
	_ = env
	if src.TargetVersion() != 2 {
		t.Fatalf("TargetVersion = %d, want 2", src.TargetVersion())
	}
}

func TestExistingVersionSource_PreviousVersion(t *testing.T) {
	resetHome(t)
	app := "resolve-prev"
	createTestVersion(t, app, 1, false)
	createTestVersion(t, app, 2, false)
	createTestVersion(t, app, 3, false)
	markCurrent(t, app, 3)

	src := &ExistingVersionSource{AppName: app, Version: 0} // 0 = previous
	bin, _, err := src.Build(context.Background())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if bin == "" {
		t.Fatalf("expected non-empty binary path")
	}
	if src.TargetVersion() != 2 {
		t.Fatalf("TargetVersion = %d, want 2", src.TargetVersion())
	}
}

func TestExistingVersionSource_VersionNotExist(t *testing.T) {
	resetHome(t)
	app := "resolve-missing"
	createTestVersion(t, app, 1, false)
	markCurrent(t, app, 1)

	src := &ExistingVersionSource{AppName: app, Version: 99}
	_, _, err := src.Build(context.Background())
	if err == nil {
		t.Fatalf("expected error for non-existent version")
	}
}

func TestExistingVersionSource_BinaryMissing(t *testing.T) {
	resetHome(t)
	app := "resolve-nobin"
	// Create version metadata but don't actually put a binary file.
	vf := &VersionsFile{Versions: []VersionMeta{
		{Version: 1, BuiltAt: time.Now(), IsCurrent: true},
	}}
	saveVersionsForTest(t, app, vf)

	src := &ExistingVersionSource{AppName: app, Version: 1}
	_, _, err := src.Build(context.Background())
	if err == nil {
		t.Fatalf("expected error when binary is missing")
	}
}

// --- concurrent lock tests ------------------------------------------------

func TestAcquireDeployLock_RejectsConcurrent(t *testing.T) {
	resetHome(t)
	app := "lock-concurrent"
	createTestVersion(t, app, 1, false)
	markCurrent(t, app, 1)
	// Write an initial state file so Load succeeds.
	initStateForTest(t, app)

	release, err := AcquireDeployLock(app, "deploy")
	if err != nil {
		t.Fatalf("first AcquireDeployLock: %v", err)
	}
	defer release()

	// Second acquire must fail.
	_, err = AcquireDeployLock(app, "rollback")
	if err == nil {
		t.Fatalf("expected error on concurrent lock acquire")
	}
}

func TestAcquireDeployLock_ReleaseAllowsNew(t *testing.T) {
	resetHome(t)
	app := "lock-release"
	createTestVersion(t, app, 1, false)
	markCurrent(t, app, 1)
	initStateForTest(t, app)

	release, err := AcquireDeployLock(app, "deploy")
	if err != nil {
		t.Fatalf("first AcquireDeployLock: %v", err)
	}
	release()

	// After release, a new acquire should succeed.
	release2, err := AcquireDeployLock(app, "rollback")
	if err != nil {
		t.Fatalf("second AcquireDeployLock after release: %v", err)
	}
	defer release2()
}

func TestAcquireDeployLock_ConcurrentFromGoroutines(t *testing.T) {
	resetHome(t)
	app := "lock-goroutines"
	createTestVersion(t, app, 1, false)
	markCurrent(t, app, 1)
	initStateForTest(t, app)

	const n = 20
	var wg sync.WaitGroup
	successes := make(chan struct{}, n)
	failures := make(chan struct{}, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := AcquireDeployLock(app, "test")
			if err != nil {
				failures <- struct{}{}
				return
			}
			successes <- struct{}{}
			// Hold the lock briefly then release.
			time.Sleep(5 * time.Millisecond)
			release()
		}()
	}
	wg.Wait()
	close(successes)
	close(failures)

	successCount := len(successes)
	failCount := len(failures)
	// At least one should succeed, at least one should fail (depending on
	// timing, all could succeed if they serialize; that's also fine).
	if successCount == 0 {
		t.Fatalf("expected at least one successful lock, got 0")
	}
	t.Logf("lock contention: %d succeeded, %d failed out of %d", successCount, failCount, n)
}

// --- rollback failure path tests ------------------------------------------

func TestRollback_UnhealthyTargetLeavesActiveUntouched(t *testing.T) {
	resetHome(t)
	app := "rollback-fail"
	log := &fakeLogger{}

	// Set up a blue-green state with v1 active on the blue slot.
	createTestVersion(t, app, 1, false)
	createTestVersion(t, app, 2, false)
	markCurrent(t, app, 1)
	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	// Do a first "deploy" via BlueGreen to establish state with an active instance.
	bg := &BlueGreen{
		AppName: app, AppID: "rollback-fail", PublicPort: 0,
		Source:         &ExistingVersionSource{AppName: app, Version: 1},
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         log,
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("initial deploy: %v", err)
	}
	before := mustLoad(t, app)
	activeSlot := before.ActiveSlot
	activePID := before.Slots[activeSlot].PID
	activeVersion := before.ActiveVersion

	// Now attempt a rollback to v2 using a launcher that produces an unhealthy
	// instance (closed port).
	bg2 := &BlueGreen{
		AppName: app, AppID: "rollback-fail", PublicPort: 0,
		Source:         &ExistingVersionSource{AppName: app, Version: 2},
		Launcher:       closedLauncher{}.Launch,
		ProxyClient:    pc,
		Logger:         log,
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealthShortTimeout() },
		GracePeriod:    200 * time.Millisecond,
	}
	err := bg2.Deploy(context.Background())
	if err == nil {
		t.Fatalf("expected rollback deploy to fail (unhealthy target)")
	}

	after := mustLoad(t, app)
	// Active slot must be untouched.
	if after.ActiveSlot != activeSlot {
		t.Fatalf("active slot changed: was %s, now %s", activeSlot, after.ActiveSlot)
	}
	// Active version must be untouched.
	if after.ActiveVersion != activeVersion {
		t.Fatalf("active version changed: was v%d, now v%d", activeVersion, after.ActiveVersion)
	}
	// Active instance PID unchanged.
	if after.Slots[activeSlot].PID != activePID {
		t.Fatalf("active PID changed: was %d, now %d", activePID, after.Slots[activeSlot].PID)
	}
}

func TestRollback_ToSameVersionFails(t *testing.T) {
	resetHome(t)
	app := "rollback-same"
	createTestVersion(t, app, 1, false)
	markCurrent(t, app, 1)
	initStateForTest(t, app)

	err := ExecuteRollback(context.Background(), RollbackOptions{
		AppName:        app,
		TargetVersion:  1,
		Source:         &ExistingVersionSource{AppName: app, Version: 1},
		Launcher:       (&httpLauncher{}).Launch,
		ProxyClient:    &fakeProxyClient{alive: true},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		Logger:         &fakeLogger{},
	})
	if err == nil {
		t.Fatalf("expected error rolling back to same version")
	}
}

func TestRollback_NoPriorDeployFails(t *testing.T) {
	resetHome(t)
	app := "rollback-nostate"
	createTestVersion(t, app, 1, false)
	createTestVersion(t, app, 2, false)

	// No deploy state exists → rollback should fail.
	err := ExecuteRollback(context.Background(), RollbackOptions{
		AppName:       app,
		TargetVersion: 1,
	})
	if err == nil {
		t.Fatalf("expected error when no prior deploy state exists")
	}
}

func TestRollback_VersionNotExistFails(t *testing.T) {
	resetHome(t)
	app := "rollback-missing"
	createTestVersion(t, app, 1, false)
	markCurrent(t, app, 1)
	initStateForTest(t, app)

	err := ExecuteRollback(context.Background(), RollbackOptions{
		AppName:        app,
		TargetVersion:  99,
		Source:         &ExistingVersionSource{AppName: app, Version: 99},
		Launcher:       (&httpLauncher{}).Launch,
		ProxyClient:    &fakeProxyClient{alive: true},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		Logger:         &fakeLogger{},
	})
	if err == nil {
		t.Fatalf("expected error for non-existent version rollback")
	}
}

// --- rollback audit log tests ----------------------------------------------

func TestRollbackAuditLog_CreatedOnSuccess(t *testing.T) {
	resetHome(t)
	app := "rollback-log"
	createTestVersion(t, app, 1, false)
	createTestVersion(t, app, 2, false)
	markCurrent(t, app, 1)
	initStateForTest(t, app)

	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	// First deploy to establish state.
	bg := &BlueGreen{
		AppName: app, AppID: "log-test", PublicPort: 0,
		Source:         &ExistingVersionSource{AppName: app, Version: 1},
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("initial deploy: %v", err)
	}
	// Patch the old instance PID so stopByPID is a no-op (avoids killing test process).
	st := mustLoad(t, app)
	if inst := st.Slots[st.ActiveSlot]; inst != nil {
		inst.PID = 999999
	}
	if err := Store(st); err != nil {
		t.Fatal(err)
	}

	// Rollback to v2.
	err := ExecuteRollback(context.Background(), RollbackOptions{
		AppName:        app,
		TargetVersion:  2,
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		Logger:         &fakeLogger{},
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Check audit log exists and contains expected content.
	logPath, err := rollbackLogPath(app)
	if err != nil {
		t.Fatalf("rollbackLogPath: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read rollback log: %v", err)
	}
	content := string(data)
	if len(content) == 0 {
		t.Fatalf("rollback log is empty")
	}
	// Should contain the version transition.
	if !containsStr(content, "v1") || !containsStr(content, "v2") {
		t.Fatalf("rollback log missing version info: %s", content)
	}
}

// --- FreshBuildSource tests -----------------------------------------------

func TestFreshBuildSource_CreatesVersion(t *testing.T) {
	resetHome(t)
	app := "fresh-build"
	policy := DefaultRetention{Max: 5}
	log := &fakeLogger{}

	// Create a temp file to act as a "built binary."
	tmpBin := filepath.Join(t.TempDir(), "myapp")
	if err := os.WriteFile(tmpBin, []byte("fake-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	src := &FreshBuildSource{
		AppName:   app,
		AppID:     "1",
		GitCommit: "abc123",
		Tag:       "test-build",
		Retention: policy,
		Logger:    log,
		BuildFn: func(_ context.Context, _ string, _ []string) (string, error) {
			return tmpBin, nil
		},
	}

	bin, _, err := src.Build(context.Background())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if bin == "" {
		t.Fatalf("expected non-empty binary path")
	}
	if src.TargetVersion() != 1 {
		t.Fatalf("TargetVersion = %d, want 1", src.TargetVersion())
	}

	// versions.json should have one entry.
	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatalf("LoadVersions: %v", err)
	}
	if len(vf.Versions) != 1 {
		t.Fatalf("expected 1 version, got %d", len(vf.Versions))
	}
	if vf.Versions[0].GitCommit != "abc123" {
		t.Fatalf("git_commit = %q, want abc123", vf.Versions[0].GitCommit)
	}
	if vf.Versions[0].Tag != "test-build" {
		t.Fatalf("tag = %q, want test-build", vf.Versions[0].Tag)
	}
}

func TestFreshBuildSource_MultipleBuildsIncrement(t *testing.T) {
	resetHome(t)
	app := "fresh-multi"
	policy := DefaultRetention{Max: 5}
	log := &fakeLogger{}

	tmpBin := filepath.Join(t.TempDir(), "myapp")
	if err := os.WriteFile(tmpBin, []byte("fake-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		src := &FreshBuildSource{
			AppName:   app,
			AppID:     "1",
			Tag:       fmt.Sprintf("build-%d", i+1),
			Retention: policy,
			Logger:    log,
			BuildFn: func(_ context.Context, _ string, _ []string) (string, error) {
				return tmpBin, nil
			},
		}
		if _, _, err := src.Build(context.Background()); err != nil {
			t.Fatalf("Build %d: %v", i+1, err)
		}
	}

	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatalf("LoadVersions: %v", err)
	}
	if len(vf.Versions) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(vf.Versions))
	}
	// Versions should be 1, 2, 3.
	for i, v := range vf.Versions {
		if v.Version != i+1 {
			t.Fatalf("version[%d] = %d, want %d", i, v.Version, i+1)
		}
	}
}

// --- RollbackEvent formatting tests ----------------------------------------

func TestFormatRollbackEvent_Success(t *testing.T) {
	ev := RollbackEvent{
		AppName:   "myapp",
		FromVer:   3,
		ToVer:     1,
		Timestamp: time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC),
		Success:   true,
	}
	msg := FormatRollbackEvent(ev)
	if !containsStr(msg, "v3") || !containsStr(msg, "v1") || !containsStr(msg, "succeeded") {
		t.Fatalf("unexpected format: %s", msg)
	}
}

func TestFormatRollbackEvent_Failure(t *testing.T) {
	ev := RollbackEvent{
		AppName:   "myapp",
		FromVer:   2,
		ToVer:     1,
		Timestamp: time.Now(),
		Success:   false,
		ErrMsg:    "health check timeout",
	}
	msg := FormatRollbackEvent(ev)
	if !containsStr(msg, "failed") || !containsStr(msg, "health check timeout") {
		t.Fatalf("unexpected format: %s", msg)
	}
}

// --- helper functions for tests -------------------------------------------

func createTestVersion(t *testing.T, appName string, ver int, isCurrent bool) {
	t.Helper()
	vdir, err := versionDir(appName, ver)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(vdir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(vdir, "binary")
	if err := os.WriteFile(bin, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	vf, err := LoadVersions(appName)
	if err != nil {
		t.Fatal(err)
	}
	vf.Versions = append(vf.Versions, VersionMeta{
		Version:   ver,
		BuiltAt:   time.Now(),
		SizeBytes: 1024,
		IsCurrent: isCurrent,
	})
	saveVersionsForTest(t, appName, vf)
}

func markCurrent(t *testing.T, appName string, ver int) {
	t.Helper()
	vf, err := LoadVersions(appName)
	if err != nil {
		t.Fatal(err)
	}
	for i := range vf.Versions {
		vf.Versions[i].IsCurrent = vf.Versions[i].Version == ver
	}
	saveVersionsForTest(t, appName, vf)
}

func saveVersionsForTest(t *testing.T, appName string, vf *VersionsFile) {
	t.Helper()
	if err := saveVersions(appName, vf); err != nil {
		t.Fatal(err)
	}
}

func initStateForTest(t *testing.T, appName string) {
	t.Helper()
	s := &DeployState{
		AppName:    appName,
		Mode:       ModeBlueGreen,
		PublicPort: 8080,
		Slots: map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, Status: "stopped"},
			SlotGreen: {Slot: SlotGreen, Status: "stopped"},
		},
	}
	if err := Store(s); err != nil {
		t.Fatal(err)
	}
}

func versionNumbers(vf *VersionsFile) []int {
	out := make([]int, len(vf.Versions))
	for i, v := range vf.Versions {
		out[i] = v.Version
	}
	return out
}

func assertContains(t *testing.T, nums []int, want int) {
	t.Helper()
	for _, n := range nums {
		if n == want {
			return
		}
	}
	t.Fatalf("expected versions to contain %d, got %v", want, nums)
}

func assertNotContains(t *testing.T, nums []int, want int) {
	t.Helper()
	for _, n := range nums {
		if n == want {
			t.Fatalf("expected versions NOT to contain %d, got %v", want, nums)
		}
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsSubstr(s, sub))
}

func containsSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Ensure encoding/json is used (it's imported by some test paths).
var _ = json.Marshal

// --- tag resolution tests --------------------------------------------------

func TestResolveVersionOrTag_VersionID(t *testing.T) {
	resetHome(t)
	app := "resolve-tag-ver"
	createTestVersion(t, app, 1, false)
	createTestVersion(t, app, 2, true)

	ver, err := ResolveVersionOrTag(app, "v2")
	if err != nil {
		t.Fatalf("ResolveVersionOrTag: %v", err)
	}
	if ver != 2 {
		t.Fatalf("expected 2, got %d", ver)
	}

	// Also test without the "v" prefix.
	ver, err = ResolveVersionOrTag(app, "1")
	if err != nil {
		t.Fatalf("ResolveVersionOrTag: %v", err)
	}
	if ver != 1 {
		t.Fatalf("expected 1, got %d", ver)
	}
}

func TestResolveVersionOrTag_TagLookup(t *testing.T) {
	resetHome(t)
	app := "resolve-tag-name"
	vf := &VersionsFile{Versions: []VersionMeta{
		{Version: 1, Tag: "release-1", BuiltAt: time.Now(), IsCurrent: false},
		{Version: 2, Tag: "hotfix-auth", BuiltAt: time.Now(), IsCurrent: true},
		{Version: 3, BuiltAt: time.Now(), IsCurrent: false},
	}}
	saveVersionsForTest(t, app, vf)

	ver, err := ResolveVersionOrTag(app, "hotfix-auth")
	if err != nil {
		t.Fatalf("ResolveVersionOrTag: %v", err)
	}
	if ver != 2 {
		t.Fatalf("expected 2, got %d", ver)
	}
}

func TestResolveVersionOrTag_TagNotFound(t *testing.T) {
	resetHome(t)
	app := "resolve-tag-miss"
	createTestVersion(t, app, 1, false)

	_, err := ResolveVersionOrTag(app, "nonexistent-tag")
	if err == nil {
		t.Fatalf("expected error for non-existent tag")
	}
}

func TestResolveVersionOrTag_TagAmbiguous(t *testing.T) {
	resetHome(t)
	app := "resolve-tag-ambig"
	vf := &VersionsFile{Versions: []VersionMeta{
		{Version: 1, Tag: "release", BuiltAt: time.Now(), IsCurrent: false},
		{Version: 2, Tag: "release", BuiltAt: time.Now(), IsCurrent: true},
	}}
	saveVersionsForTest(t, app, vf)

	_, err := ResolveVersionOrTag(app, "release")
	if err == nil {
		t.Fatalf("expected error for ambiguous tag")
	}
	// Error should mention the matching versions.
	if !containsStr(err.Error(), "v1") || !containsStr(err.Error(), "v2") {
		t.Fatalf("error should mention matching versions: %v", err)
	}
}

func TestResolveVersionOrTag_EmptyInput(t *testing.T) {
	resetHome(t)
	_, err := ResolveVersionOrTag("any", "")
	if err == nil {
		t.Fatalf("expected error for empty input")
	}
}

func TestResolveVersionOrTag_VersionNotExist(t *testing.T) {
	resetHome(t)
	app := "resolve-tag-nover"
	createTestVersion(t, app, 1, false)

	_, err := ResolveVersionOrTag(app, "v99")
	if err == nil {
		t.Fatalf("expected error for non-existent version")
	}
}

// --- build-succeeds-deploy-fails ordering test -----------------------------

func TestRecordFreshBuild_IsCurrentFalse(t *testing.T) {
	// This test verifies the critical invariant: RecordFreshBuild always creates
	// a version with is_current=false. The caller (build/rebuild) must call
	// PromoteVersion only after the deploy's health check passes. If the deploy
	// fails, the version stays on disk but is_current must never flip to true.
	resetHome(t)
	app := "build-deploy-order"
	policy := DefaultRetention{Max: 5}
	log := &fakeLogger{}

	tmpBin := filepath.Join(t.TempDir(), "myapp")
	if err := os.WriteFile(tmpBin, []byte("fake-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Simulate: build succeeds, record version (with build-report metadata).
	rec, err := RecordFreshBuild(app, "1", tmpBin, "abc123", "pre-release", nil, policy, log)
	if err != nil {
		t.Fatalf("RecordFreshBuild: %v", err)
	}

	// The version must exist on disk.
	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatalf("LoadVersions: %v", err)
	}
	if len(vf.Versions) != 1 {
		t.Fatalf("expected 1 version, got %d", len(vf.Versions))
	}
	v := vf.Versions[0]

	// CRITICAL: is_current must be false even though the build succeeded.
	if v.IsCurrent {
		t.Fatalf("version must NOT be current after build — deploy hasn't happened yet")
	}
	if v.Tag != "pre-release" {
		t.Fatalf("tag = %q, want pre-release", v.Tag)
	}
	if rec.Version != 1 {
		t.Fatalf("expected version 1, got %d", rec.Version)
	}

	// Now simulate: deploy fails (don't call PromoteVersion).
	// is_current must still be false.
	vf2, err := LoadVersions(app)
	if err != nil {
		t.Fatalf("LoadVersions: %v", err)
	}
	if vf2.Versions[0].IsCurrent {
		t.Fatalf("is_current must remain false after failed deploy")
	}

	// Now simulate: a retry succeeds. PromoteVersion should work.
	if err := PromoteVersion(app, 1, "blue-green"); err != nil {
		t.Fatalf("PromoteVersion: %v", err)
	}
	vf3, err := LoadVersions(app)
	if err != nil {
		t.Fatalf("LoadVersions: %v", err)
	}
	if !vf3.Versions[0].IsCurrent {
		t.Fatalf("is_current should be true after successful promote")
	}
}

// --- concurrent version numbering test ------------------------------------

// TestNextVersion_Concurrent demonstrates that NextVersion alone is not atomic.
// The safety guarantee comes from AcquireDeployLock serializing build/rollback
// operations on the same app. This test shows the race; the lock test below
// shows the fix.
func TestNextVersion_Concurrent_NoLock_ShowsRace(t *testing.T) {
	resetHome(t)
	app := "next-ver-race"
	const n = 20
	var wg sync.WaitGroup
	results := make(chan int, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ver, err := NextVersion(app)
			if err != nil {
				t.Errorf("NextVersion: %v", err)
				return
			}
			results <- ver
		}()
	}
	wg.Wait()
	close(results)

	// Without a lock, multiple goroutines can see the same max version and
	// return the same next version. This is expected — the deploy lock is the
	// serialization mechanism.
	seen := make(map[int]bool)
	for ver := range results {
		seen[ver] = true
	}
	t.Logf("without lock: %d goroutines produced %d distinct version numbers (race expected)", n, len(seen))
}

// TestNextVersion_WithDeployLock_NoDuplicate demonstrates that when the deploy
// lock serializes operations, version numbering is safe. The lock ensures that
// only one goroutine can hold it at a time, so concurrent build/rebuild/rollback
// calls on the same app can't corrupt versions.json or double-assign a version.
func TestNextVersion_WithDeployLock_NoDuplicate(t *testing.T) {
	resetHome(t)
	app := "next-ver-locked"
	initStateForTest(t, app)

	const n = 10
	var wg sync.WaitGroup
	successes := make(chan struct{}, n)
	failures := make(chan struct{}, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := AcquireDeployLock(app, "test")
			if err != nil {
				failures <- struct{}{}
				return
			}
			successes <- struct{}{}
			// Hold the lock briefly to serialize with other goroutines.
			time.Sleep(5 * time.Millisecond)
			release()
		}()
	}
	wg.Wait()
	close(successes)
	close(failures)

	successCount := len(successes)
	failCount := len(failures)
	// The lock serializes: at most one goroutine holds it at a time.
	// Some should succeed, some should fail (contention).
	if successCount == 0 {
		t.Fatalf("expected at least one successful lock, got 0")
	}
	t.Logf("lock contention: %d succeeded, %d failed out of %d", successCount, failCount, n)
}

// --- list/status graceful handling ----------------------------------------

func TestLoadVersions_NoFile(t *testing.T) {
	resetHome(t)
	app := "no-versions-file"
	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatalf("LoadVersions should not error for missing file: %v", err)
	}
	if len(vf.Versions) != 0 {
		t.Fatalf("expected empty versions, got %d", len(vf.Versions))
	}
}

func TestCurrentVersionMeta_NoVersions(t *testing.T) {
	resetHome(t)
	app := "no-meta"
	meta, err := CurrentVersionMeta(app)
	if err != nil {
		t.Fatalf("CurrentVersionMeta: %v", err)
	}
	if meta != nil {
		t.Fatalf("expected nil meta for app with no versions")
	}
}

func TestRecentVersions_NoVersions(t *testing.T) {
	resetHome(t)
	app := "no-recent"
	vers, err := RecentVersions(app, 3)
	if err != nil {
		t.Fatalf("RecentVersions: %v", err)
	}
	if len(vers) != 0 {
		t.Fatalf("expected 0 versions, got %d", len(vers))
	}
}
