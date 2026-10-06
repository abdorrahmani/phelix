package cmd

// context_test.go pins the Phase 2 inspection/context contract: facts only,
// bounded output with explicit truncation, redacted messages, deterministic
// ordering, and the Phase 1 envelope on pure stdout.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/ops"
)

// contextDataDir isolates the data dir per test (the ops store, log paths and
// app state all re-resolve per call, mirroring Phase 1 test practice).
func contextDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	return dir
}

func TestValidateContextLimit(t *testing.T) {
	def, err := validateContextLimit("versions", 0, contextDefaultVersions, contextMaxVersions)
	if err != nil || def != contextDefaultVersions {
		t.Fatalf("0 must select the default, got %d/%v", def, err)
	}
	if def, err = validateContextLimit("versions", 5, 20, 100); err != nil || def != 5 {
		t.Fatalf("explicit limit must pass through, got %d/%v", def, err)
	}
	if _, err := validateContextLimit("versions", -1, 20, 100); !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
		t.Fatalf("negative limit must be rejected, got %v", err)
	}
	if _, err := validateContextLimit("versions", 101, 20, 100); !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
		t.Fatalf("over-cap limit must be rejected, not clamped, got %v", err)
	}
}

func TestProjectContext_ValidProject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nimport \"os\"\n\nfunc main() { _ = os.Getenv(\"PORT\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "phelix.yaml"), []byte("name: demo\nport: 9000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	view, err := buildProjectContext(dir)
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}
	if view.Root != dir || view.Language != "go" || view.Name != "demo" || view.Port != 9000 {
		t.Fatalf("project facts wrong: %+v", view)
	}
	if !view.ConfigFound || !view.ConfigValid {
		t.Fatalf("valid config must be found+valid: %+v", view)
	}
	if !view.ReadsPort {
		t.Fatalf("project reading PORT must report reads_port: %+v", view)
	}
}

func TestProjectContext_MissingProject(t *testing.T) {
	_, err := buildProjectContext(t.TempDir())
	if !phelixerr.IsCode(err, phelixerr.CodeNotFound) {
		t.Fatalf("empty dir must be NOT_FOUND, got %v", err)
	}
}

func TestProjectContext_InvalidConfigIsAFact(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "phelix.yaml"), []byte("name: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	view, err := buildProjectContext(dir)
	if err != nil {
		t.Fatalf("an invalid config is a fact, not a failed inspection: %v", err)
	}
	if !view.ConfigFound || view.ConfigValid {
		t.Fatalf("malformed config must report found=true valid=false: %+v", view)
	}
	if view.ConfigError == "" {
		t.Fatal("config_error must carry the (redacted) reason")
	}
}

func TestRuntimeContext_FactsOnly(t *testing.T) {
	view := buildRuntimeContext()
	if view.OS == "" || view.Architecture == "" {
		t.Fatal("os/arch are always facts")
	}
	if !view.Go.Installed {
		t.Skip("go toolchain not on PATH in this environment")
	}
	if view.Go.Version == "" {
		t.Fatal("installed toolchain must report its version (a fact, not a recommendation)")
	}
	if strings.Contains(strings.ToLower(view.Go.Version), "recommend") {
		t.Fatalf("runtime context must not contain recommendations: %q", view.Go.Version)
	}
}

func TestDeploymentContext_NoStateIsPending(t *testing.T) {
	view := buildDeploymentContext(nil)
	if view.Status != machine.StatusPending || view.Source != "persisted" {
		t.Fatalf("no deploy.json must render as pending/persisted: %+v", view)
	}
}

func TestDeploymentContext_OpLockMeansRunning(t *testing.T) {
	state := &deploy.DeployState{
		AppName:          "demo",
		Mode:             deploy.ModeBlueGreen,
		PublicPort:       9000,
		ActiveVersion:    3,
		ActiveSlot:       "blue",
		LastDeploymentID: "dep-0123456789abcdef01234567",
		OpLock: &deploy.DeployLock{
			Operation: "deploy",
			PID:       4242,
			StartedAt: time.Now(),
		},
		UpdatedAt: time.Now(),
	}
	view := buildDeploymentContext(state)
	if view.Status != machine.StatusRunning {
		t.Fatalf("an in-flight op lock must surface as running, got %q", view.Status)
	}
	if view.OpLock == nil || view.OpLock.PID != 4242 {
		t.Fatalf("op lock details must be projected: %+v", view.OpLock)
	}
	if view.DeploymentID != "dep-0123456789abcdef01234567" {
		t.Fatalf("deployment id correlation lost: %q", view.DeploymentID)
	}
	if view.Strategy != "blue-green" || view.ActiveVersion != 3 {
		t.Fatalf("strategy/version facts lost: %+v", view)
	}
}

func TestDeploymentContext_CanaryAndRollbackFacts(t *testing.T) {
	state := &deploy.DeployState{
		AppName: "demo",
		Mode:    deploy.ModeBlueGreen,
		Canary: &deploy.CanaryState{
			Status:   "running",
			Strategy: "canary",
			Version:  7,
			Step:     1,
			Steps:    3,
		},
		LastRollback: &deploy.RollbackRecord{
			FromVersion: 6,
			ToVersion:   5,
			At:          time.Now(),
		},
		UpdatedAt: time.Now(),
	}
	view := buildDeploymentContext(state)
	if view.Canary == nil || view.Canary.Status != "running" || view.Canary.Version != 7 {
		t.Fatalf("canary state must be projected: %+v", view.Canary)
	}
	if view.LastRollback == nil || view.LastRollback.ToVersion != 5 {
		t.Fatalf("rollback fact must be projected: %+v", view.LastRollback)
	}
}

func TestVersionsContext_BoundedAndTruncated(t *testing.T) {
	contextDataDir(t)
	dir := t.TempDir()
	appName := "demo"
	// Seed enough versions for the retention policy to settle and the limit
	// to bind (RecordFreshBuild prunes to DefaultRetention as it records).
	for i := 0; i < 8; i++ {
		bin := filepath.Join(dir, fmt.Sprintf("bin%d", i))
		if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := deploy.RecordFreshBuild(appName, "id-demo", bin, "", "", nil,
			deploy.DefaultRetention{Max: 5}, &colorLogger{}); err != nil {
			t.Fatalf("RecordFreshBuild: %v", err)
		}
	}

	view, err := buildVersionsContext(appName, 3)
	if err != nil {
		t.Fatalf("buildVersionsContext: %v", err)
	}
	if view.Count != 3 || !view.Truncated || view.Limit != 3 {
		t.Fatalf("limit=3 over 5 versions must be bounded+truncated: %+v", view)
	}
	// Nothing was ever promoted, so Current is legitimately absent — a fact,
	// not an error.
	if view.Current != nil {
		t.Fatalf("never-deployed versions must not claim a current: %+v", view.Current)
	}

	full, err := buildVersionsContext(appName, contextDefaultVersions)
	if err != nil {
		t.Fatalf("buildVersionsContext(default): %v", err)
	}
	if full.Truncated {
		t.Fatalf("retained versions under a 20 limit are not truncated: count=%d", full.Count)
	}
}

func TestHealthContext_Rollup(t *testing.T) {
	cases := []struct {
		name   string
		checks []healthCheckView
		want   string
	}{
		{"no checks", nil, HealthStatusUnknown},
		{"all up", []healthCheckView{{Name: "a", Status: "UP"}, {Name: "b", Status: "UP"}}, HealthStatusHealthy},
		{"any down", []healthCheckView{{Name: "a", Status: "UP"}, {Name: "b", Status: "DOWN"}}, HealthStatusUnhealthy},
		{"timeout degrades", []healthCheckView{{Name: "a", Status: "TIMEOUT"}}, HealthStatusDegraded},
	}
	for _, tc := range cases {
		if got := rollupHealthStatus(tc.checks, "oneshot"); got != tc.want {
			t.Errorf("%s: rollup = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestHealthContext_OneShotSortedAndRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`bad key sk-abcdef0123456789012345678901234567890123456`))
	}))
	defer srv.Close()

	contextDataDir(t)
	appID := seedHealthConfig(t, "health-demo", "http://127.0.0.1:1/probe")

	view, err := buildHealthContext(appID, "demo")
	if err != nil {
		t.Fatalf("buildHealthContext: %v", err)
	}
	if view.Source != "oneshot" {
		t.Fatalf("no daemon in tests: source must be oneshot, got %q", view.Source)
	}
	for i := 1; i < len(view.Checks); i++ {
		if view.Checks[i-1].Name > view.Checks[i].Name {
			t.Fatal("checks must be sorted by name for deterministic output")
		}
	}
	for _, c := range view.Checks {
		if c.Error != nil && strings.Contains(*c.Error, "sk-abcdef") {
			t.Fatalf("health failure message leaked a credential: %q", *c.Error)
		}
	}
	if view.Status == HealthStatusHealthy {
		t.Fatalf("one unreachable + one 401 endpoint must not roll up healthy: %+v", view.Checks)
	}
}

func TestOperationsContext_BoundedAndTruncated(t *testing.T) {
	contextDataDir(t)
	for i := 0; i < 4; i++ {
		rec, err := ops.Begin(ops.KindRebuild, "demo", "")
		if err != nil {
			t.Fatalf("ops Begin: %v", err)
		}
		_ = rec.MarkSucceeded(nil)
	}
	view, err := buildOperationsContext("demo", 2)
	if err != nil {
		t.Fatalf("buildOperationsContext: %v", err)
	}
	if view.Count != 2 || !view.Truncated {
		t.Fatalf("limit=2 over 4 ops must be bounded+truncated: %+v", view)
	}
	full, err := buildOperationsContext("demo", contextDefaultOperations)
	if err != nil {
		t.Fatalf("buildOperationsContext(default): %v", err)
	}
	if full.Count != 4 || full.Truncated {
		t.Fatalf("4 ops under a 20 limit are not truncated: %+v", full)
	}
}

func TestLogsContext_RedactsAndBounds(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("2026/10/06 10:%02d:00 [INFO] [app] line %d password=supersecret%d", i, i, i))
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	view, err := buildLogsContext(logViewSpec{Source: "app", Name: "demo", Path: logPath}, 10)
	if err != nil {
		t.Fatalf("buildLogsContext: %v", err)
	}
	if view.Count != 10 || !view.Truncated {
		t.Fatalf("bounded read must return 10 items with truncated=true (got count=%d truncated=%v)", view.Count, view.Truncated)
	}
	for _, item := range view.Items {
		if strings.Contains(item, "supersecret") {
			t.Fatalf("log context leaked a credential: %q", item)
		}
	}
}

func TestContextJSON_PureStdoutEnvelope_NoSecretLeak(t *testing.T) {
	contextDataDir(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nimport \"os\"\n\nfunc main() { _ = os.Getenv(\"PORT\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "phelix.yaml"), []byte("name: demo\nport: 9000\nwebhook:\n  enabled: true\n  branch: main\n  secret_env: PHELIX_WEBHOOK_SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	contextJSONPrev := contextJSON
	contextJSON = true
	defer func() { contextJSON = contextJSONPrev }()
	defer machine.LeaveJSON()

	prevWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(prevWd) }()

	out := captureStdout(t, func() {
		if err := ContextCmd.RunE(ContextCmd, nil); err != nil {
			t.Errorf("context in a valid project: %v", err)
		}
	})

	env := decodeEnvelope(t, out)
	if env.Status != machine.StatusSucceeded {
		t.Fatalf("status = %q", env.Status)
	}
	if env.OperationID != "" {
		t.Fatal("context is read-only: it must never fabricate an operation id")
	}
	raw, _ := json.Marshal(env.Result)
	var result contextResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("result is not a contextResult: %v", err)
	}
	if result.Project == nil || result.Runtime == nil || result.Capabilities == nil {
		t.Fatalf("project/runtime/capabilities are always present: %+v", result)
	}
	if len(result.Capabilities.Capabilities) == 0 {
		t.Fatal("this build registers capabilities; the list must not be empty")
	}
	for i := 1; i < len(result.Capabilities.Capabilities); i++ {
		if result.Capabilities.Capabilities[i-1] > result.Capabilities.Capabilities[i] {
			t.Fatal("capabilities must be deterministically sorted")
		}
	}
	if result.Config == nil || result.Config.Webhook == nil {
		t.Fatalf("webhook config must be projected: %+v", result.Config)
	}
	if result.Config.Webhook.SecretEnv != "PHELIX_WEBHOOK_SECRET" {
		t.Fatalf("secret_env must carry the env var NAME: %q", result.Config.Webhook.SecretEnv)
	}
	if strings.Contains(string(raw), "supersecret") {
		t.Fatal("context leaked a secret value")
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatal("context stdout must never contain ANSI escapes")
	}
}

func TestContextJSON_AppSectionsOnlyWhenNamed(t *testing.T) {
	contextDataDir(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	contextJSONPrev := contextJSON
	contextJSON = true
	defer func() { contextJSON = contextJSONPrev }()
	defer machine.LeaveJSON()

	prevWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(prevWd) }()

	out := captureStdout(t, func() {
		if err := ContextCmd.RunE(ContextCmd, nil); err != nil {
			t.Errorf("context without app: %v", err)
		}
	})
	env := decodeEnvelope(t, out)
	raw, _ := json.Marshal(env.Result)
	var result contextResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Application != nil || result.Deployment != nil || result.Versions != nil ||
		result.Health != nil || result.Operations != nil || result.Logs != nil {
		t.Fatalf("app-scoped sections must be omitted without an app argument: %+v", result)
	}
	if result.Project == nil || result.Runtime == nil {
		t.Fatal("environment sections must still be present")
	}
}

func TestInspectJSON_PureStdout(t *testing.T) {
	contextDataDir(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	capabilitiesPrev := inspectCapabilitiesJSON
	inspectCapabilitiesJSON = true
	defer func() { inspectCapabilitiesJSON = capabilitiesPrev }()
	defer machine.LeaveJSON()

	out := captureStdout(t, func() {
		if err := inspectCapabilitiesCmd.RunE(inspectCapabilitiesCmd, nil); err != nil {
			t.Errorf("inspect capabilities: %v", err)
		}
	})
	env := decodeEnvelope(t, out)
	raw, _ := json.Marshal(env.Result)
	var caps inspectCapabilities
	if err := json.Unmarshal(raw, &caps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(caps.Capabilities) == 0 {
		t.Fatal("capability discovery must not be empty for this build")
	}
}

func TestAppContext_ShapeStability(t *testing.T) {
	// buildAppContext projects app.AppStatus; pin the contract surface: the
	// source is always "live" (reconciled) and the proxy view always present.
	view := buildAppContext(app.AppStatus{}, false, nil)
	if view.Source != "live" {
		t.Fatalf("app context source must be live: %+v", view)
	}
	if view.Proxy == nil {
		t.Fatal("proxy view must always be present (running=false is a fact)")
	}
}

// seedHealthConfig seeds two health endpoints for appID through the same
// config-manager API `phelix health add` uses, then returns the app ID.
func seedHealthConfig(t *testing.T, appID, failingURL string) string {
	t.Helper()
	mgr, err := health.InitConfigManager()
	if err != nil {
		t.Fatalf("InitConfigManager: %v", err)
	}
	cfg := mgr.GetConfig(appID)
	if cfg == nil {
		cfg = &health.AppHealthConfig{AppID: appID}
	}
	if cfg.Endpoints == nil {
		cfg.Endpoints = map[string]*health.HealthCheckConfig{}
	}
	cfg.Endpoints["probe-a"] = &health.HealthCheckConfig{
		Name: "probe-a", URL: "http://127.0.0.1:1/probe",
		Interval: "10s", Retries: 1, ExpectedCodes: "200-299", Timeout: "2s",
	}
	cfg.Endpoints["probe-b"] = &health.HealthCheckConfig{
		Name: "probe-b", URL: failingURL,
		Interval: "10s", Retries: 1, ExpectedCodes: "200-299", Timeout: "2s",
	}
	if err := mgr.SaveConfig(appID, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	return appID
}
