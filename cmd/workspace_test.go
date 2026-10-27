package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/workspace"
)

// workspaceTestEnv creates a workspace directory with the given manifest,
// service directories, and an isolated data dir, then points the test's cwd
// and data dir at them.
func workspaceTestEnv(t *testing.T, manifest string, serviceDirs ...string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", filepath.Join(root, "data"))
	wsDir := filepath.Join(root, "mesh")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(wsDir, workspace.FileName), []byte(manifest), 0o644); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
	for _, dir := range serviceDirs {
		if err := os.MkdirAll(filepath.Join(wsDir, dir), 0o755); err != nil {
			t.Fatalf("mkdir service %s: %v", dir, err)
		}
	}
	t.Chdir(wsDir)
	return wsDir
}

// sequentialRecorder is a mutex-guarded call-order log for fakes.
type sequentialRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *sequentialRecorder) record(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name)
}

func (r *sequentialRecorder) order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func fakeBuildOne(rec *sequentialRecorder, fail map[string]bool) workspaceBuildServiceFn {
	return func(svc *workspace.Service, _ []string, _ *builder.BuildManager) (int, error) {
		rec.record(svc.Name)
		if fail[svc.Name] {
			return 0, fmt.Errorf("compile error in %s", svc.Name)
		}
		return 3, nil
	}
}

// seedApps replaces the app registry with fresh stopped entries for the
// given service names and pins the state-loading seam to a no-op, because
// the app package pins its on-disk state path at package init and an
// in-process test cannot redirect it.
func seedApps(t *testing.T, names ...string) {
	t.Helper()
	manager, ok := app.Manager.(*app.AppManager)
	if !ok {
		t.Fatalf("invalid app manager type")
	}
	prevLoader := loadAppStateForWorkspace
	prevApps := manager.Apps
	manager.Apps = map[string]*app.AppInfo{}
	t.Cleanup(func() {
		manager.Apps = prevApps
		loadAppStateForWorkspace = prevLoader
	})
	now := time.Now()
	for _, name := range names {
		manager.Apps["id-"+name] = &app.AppInfo{
			ID:          "id-" + name,
			Name:        name,
			Status:      "stopped",
			BuildStatus: "success",
			Language:    "go",
			Directory:   name,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
	}
	loadAppStateForWorkspace = func() error { return nil }
}

func TestOrchestrateWorkspaceBuild_OrdersByDependency(t *testing.T) {
	workspaceTestEnv(t, `
services:
  auth: { path: ./auth, port: 8081 }
  orders: { path: ./orders, depends_on: [auth] }
  gateway: { path: ./gateway, depends_on: [orders] }
`, "auth", "orders", "gateway")
	seedApps(t)

	rec := &sequentialRecorder{}
	if err := orchestrateWorkspaceBuild(fakeBuildOne(rec, nil), nil); err != nil {
		t.Fatalf("orchestrateWorkspaceBuild: %v", err)
	}
	got := rec.order()
	if len(got) != 3 || got[0] != "auth" || got[1] != "orders" || got[2] != "gateway" {
		t.Fatalf("build order = %v, want auth before its dependents", got)
	}

	state, err := workspace.LoadState("mesh")
	if err != nil || state == nil {
		t.Fatalf("workspace state not written: %v", err)
	}
	auth := state.Services[0]
	if auth.Name != "auth" || auth.LastResult != "succeeded" || auth.LastVersion != 3 {
		t.Fatalf("auth state = %+v", auth)
	}
	if state.Services[0].Port != 8081 || state.Services[1].Port != 8080 || state.Services[2].Port != 8082 {
		t.Fatalf("ports not assigned as expected: %+v", state.Services)
	}
}

func TestOrchestrateWorkspaceBuild_HaltsOnFailure(t *testing.T) {
	workspaceTestEnv(t, `
services:
  auth: { path: ./auth }
  orders: { path: ./orders, depends_on: [auth] }
  gateway: { path: ./gateway, depends_on: [orders] }
`, "auth", "orders", "gateway")
	seedApps(t)

	rec := &sequentialRecorder{}
	err := orchestrateWorkspaceBuild(fakeBuildOne(rec, map[string]bool{"orders": true}), nil)
	if err == nil || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("err = %v, want a halt mentioning orders", err)
	}
	got := rec.order()
	if len(got) != 2 || got[0] != "auth" || got[1] != "orders" {
		t.Fatalf("build order = %v, want gateway skipped after orders failed", got)
	}
	state, err := workspace.LoadState("mesh")
	if err != nil || state == nil {
		t.Fatalf("workspace state not written: %v", err)
	}
	var orders *workspace.ServiceState
	for i := range state.Services {
		if state.Services[i].Name == "orders" {
			orders = &state.Services[i]
		}
	}
	if orders == nil {
		t.Fatalf("orders missing from workspace state: %+v", state.Services)
	}
	if orders.LastResult != "failed" || orders.LastError == "" {
		t.Fatalf("orders state = %+v, want a recorded failure", orders)
	}
}

func TestOrchestrateWorkspaceDeploy_RequiresRegisteredServices(t *testing.T) {
	workspaceTestEnv(t, `
services:
  auth: { path: ./auth, port: 8081 }
`, "auth")
	seedApps(t)

	rec := &sequentialRecorder{}
	deployOne := func(svc *workspace.Service, _ *app.AppInfo) (int, int, string, error) {
		rec.record(svc.Name)
		return 1, svc.Port, workspace.StrategyClassic, nil
	}
	err := orchestrateWorkspaceDeploy(deployOne)
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("err = %v, want not-registered failure", err)
	}
	if len(rec.order()) != 0 {
		t.Fatalf("no service should deploy when none are registered, got %v", rec.order())
	}
}

func TestOrchestrateWorkspaceDeploy_OrdersByDependencyAndHalts(t *testing.T) {
	workspaceTestEnv(t, `
services:
  auth: { path: ./auth, port: 8081 }
  orders: { path: ./orders, port: 8082, depends_on: [auth] }
  gateway: { path: ./gateway, port: 8083, depends_on: [orders] }
`, "auth", "orders", "gateway")
	seedApps(t, "auth", "orders", "gateway")

	rec := &sequentialRecorder{}
	fail := map[string]bool{"orders": true}
	deployOne := func(svc *workspace.Service, _ *app.AppInfo) (int, int, string, error) {
		rec.record(svc.Name)
		if fail[svc.Name] {
			return 0, svc.Port, workspace.StrategyClassic, fmt.Errorf("health check failed for %s", svc.Name)
		}
		return 7, svc.Port, workspace.StrategyClassic, nil
	}

	err := orchestrateWorkspaceDeploy(deployOne)
	if err == nil || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("err = %v, want a halt mentioning orders", err)
	}
	got := rec.order()
	if len(got) != 2 || got[0] != "auth" || got[1] != "orders" {
		t.Fatalf("deploy order = %v, want gateway skipped after orders failed", got)
	}
	state, err := workspace.LoadState("mesh")
	if err != nil || state == nil {
		t.Fatalf("workspace state not written: %v", err)
	}
	byName := map[string]workspace.ServiceState{}
	for _, svc := range state.Services {
		byName[svc.Name] = svc
	}
	if auth := byName["auth"]; auth.LastResult != "succeeded" || auth.LastVersion != 7 {
		t.Fatalf("auth state = %+v", auth)
	}
	if orders := byName["orders"]; orders.LastResult != "failed" {
		t.Fatalf("orders state = %+v", orders)
	}
}

func TestOrchestrateWorkspaceDeploy_SucceedsInOrder(t *testing.T) {
	workspaceTestEnv(t, `
services:
  auth: { path: ./auth, port: 8081, strategy: blue-green }
  gateway: { path: ./gateway, port: 8082, depends_on: [auth] }
`, "auth", "gateway")
	seedApps(t, "auth", "gateway")

	rec := &sequentialRecorder{}
	deployOne := func(svc *workspace.Service, _ *app.AppInfo) (int, int, string, error) {
		rec.record(svc.Name)
		return 2, svc.Port, "blue-green", nil
	}
	if err := orchestrateWorkspaceDeploy(deployOne); err != nil {
		t.Fatalf("orchestrateWorkspaceDeploy: %v", err)
	}
	got := rec.order()
	if len(got) != 2 || got[0] != "auth" || got[1] != "gateway" {
		t.Fatalf("deploy order = %v, want auth then gateway", got)
	}
	state, err := workspace.LoadState("mesh")
	if err != nil || state == nil {
		t.Fatalf("workspace state not written: %v", err)
	}
	if state.Services[0].LastResult != "succeeded" || state.Services[1].LastResult != "succeeded" {
		t.Fatalf("state = %+v", state.Services)
	}
}

func TestOrchestrateWorkspaceDeploy_RestoresEnvelopesAfterFailure(t *testing.T) {
	workspaceTestEnv(t, "services:\n  auth: { path: ./auth, port: 8081 }\n", "auth")
	seedApps(t)

	deployOne := func(_ *workspace.Service, _ *app.AppInfo) (int, int, string, error) {
		return 0, 0, "", fmt.Errorf("health check failed")
	}
	if err := orchestrateWorkspaceDeploy(deployOne); err == nil {
		t.Fatalf("expected a deploy failure")
	}
	if envelopeSuppressed {
		t.Fatalf("envelope suppression leaked past the failed deploy")
	}
}

func TestResolveWorkspaceStrategy_Table(t *testing.T) {
	cases := []struct {
		name       string
		manifest   string
		projYaml   string
		deployMode string
		want       string
	}{
		{name: "manifest wins", manifest: workspace.StrategyRolling, projYaml: workspace.StrategyBlueGreen, deployMode: string(deploy.ModeBlueGreen), want: workspace.StrategyRolling},
		{name: "phelix.yaml second", projYaml: workspace.StrategyBlueGreen, deployMode: string(deploy.ModeRolling), want: workspace.StrategyBlueGreen},
		{name: "deploy.json third", deployMode: string(deploy.ModeRolling), want: workspace.StrategyRolling},
		{name: "defaults to classic", want: workspace.StrategyClassic},
		{name: "canary degrades to classic", projYaml: project.StrategyCanary, want: workspace.StrategyClassic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			svc := &workspace.Service{Name: "orders", Strategy: tc.manifest}
			var projCfg *project.Config
			if tc.projYaml != "" {
				projCfg = &project.Config{Deploy: &project.DeployConfig{Strategy: tc.projYaml}}
			}
			if tc.deployMode != "" {
				dir := filepath.Join(home, ".phelix", "apps", "orders")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir app dir: %v", err)
				}
				body := fmt.Sprintf(`{"app_name":"orders","mode":%q}`, tc.deployMode)
				if err := os.WriteFile(filepath.Join(dir, "deploy.json"), []byte(body), 0o644); err != nil {
					t.Fatalf("write deploy.json: %v", err)
				}
			}
			if got := resolveWorkspaceStrategy(svc, projCfg, "orders"); got != tc.want {
				t.Fatalf("resolveWorkspaceStrategy() = %q, want %q", got, tc.want)
			}
		})
	}
}
