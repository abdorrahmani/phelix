package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/workspace"
)

// seedWorkspaceHome fabricates ~/.phelix state for one app (auth) with a
// stopped blue-green deployment and one recorded version.
func seedWorkspaceHome(t *testing.T, home, wsDir string) {
	t.Helper()
	phelixDir := filepath.Join(home, ".phelix")
	for _, dir := range []string{phelixDir, filepath.Join(phelixDir, "apps", "auth")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	apps := map[string]map[string]any{
		"a1": {
			"id": "a1", "name": "auth", "pid": 0, "status": "stopped", "port": 8081,
			"directory": filepath.Join(wsDir, "auth"), "language": "go",
			"build_status": "success", "created_at": now, "updated_at": now,
		},
	}
	appsRaw, err := json.Marshal(apps)
	if err != nil {
		t.Fatalf("marshal apps.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(phelixDir, "apps.json"), appsRaw, 0o644); err != nil {
		t.Fatalf("write apps.json: %v", err)
	}
	deployState := `{"app_name":"auth","app_id":"a1","mode":"blue-green","public_port":8081,` +
		`"active_slot":"blue","active_version":1,` +
		`"slots":{"blue":{"slot":"blue","pid":0,"port":19001,"status":"stopped","version":1}},` +
		`"updated_at":"` + now + `"}`
	if err := os.WriteFile(filepath.Join(phelixDir, "apps", "auth", "deploy.json"), []byte(deployState), 0o644); err != nil {
		t.Fatalf("write deploy.json: %v", err)
	}
	versions := `{"versions":[{"version":1,"built_at":"` + now + `","size_bytes":1024,"is_current":true,"deploy_mode":"blue-green"}]}`
	if err := os.WriteFile(filepath.Join(phelixDir, "apps", "auth", "versions.json"), []byte(versions), 0o644); err != nil {
		t.Fatalf("write versions.json: %v", err)
	}
}

// newCLIWorkspace creates a two-service workspace (auth, orders) in a fresh
// home and returns home plus the workspace dir. With seedState true, auth is
// registered with a stopped blue-green deployment and one recorded version.
func newCLIWorkspace(t *testing.T, seedState bool) (string, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	wsDir := filepath.Join(root, "mesh")
	for _, dir := range []string{home, wsDir, filepath.Join(wsDir, "auth"), filepath.Join(wsDir, "orders")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	manifest := "services:\n" +
		"  auth: { path: ./auth, port: 8081, strategy: blue-green }\n" +
		"  orders: { path: ./orders, port: 8082, depends_on: [auth] }\n"
	if err := os.WriteFile(filepath.Join(wsDir, workspace.FileName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if seedState {
		seedWorkspaceHome(t, home, wsDir)
	}
	return home, wsDir
}

func TestCLIStatusWorkspace(t *testing.T) {
	home, wsDir := newCLIWorkspace(t, true)

	code, stdout, stderr := runPhelixIn(t, home, wsDir, "status", "--workspace", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", code, ExitOK, stdout, stderr)
	}
	var envelope struct {
		SchemaVersion string `json:"schema_version"`
		Status        string `json:"status"`
		Result        struct {
			Workspace string `json:"workspace"`
			Source    string `json:"source"`
			Services  []struct {
				Name       string `json:"name"`
				Registered bool   `json:"registered"`
				Status     string `json:"status"`
				Version    int    `json:"version"`
				Port       int    `json:"port"`
				Strategy   string `json:"strategy"`
			} `json:"services"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\nstdout=%q", err, stdout)
	}
	if envelope.Status != "succeeded" {
		t.Fatalf("envelope status = %q", envelope.Status)
	}
	if envelope.Result.Workspace != "mesh" || envelope.Result.Source != "manifest" {
		t.Fatalf("workspace header = %+v", envelope.Result)
	}
	services := envelope.Result.Services
	if len(services) != 2 {
		t.Fatalf("services = %+v, want 2", services)
	}
	auth := services[0]
	if auth.Name != "auth" || !auth.Registered || auth.Status != "stopped" || auth.Version != 1 || auth.Port != 8081 {
		t.Fatalf("auth view = %+v", auth)
	}
	if auth.Strategy != "blue-green" {
		t.Fatalf("auth strategy = %q, want blue-green", auth.Strategy)
	}
	orders := services[1]
	if orders.Name != "orders" || orders.Registered || orders.Status != "not registered" {
		t.Fatalf("orders view = %+v", orders)
	}

	code, stdout, _ = runPhelixIn(t, home, wsDir, "status", "--workspace")
	if code != ExitOK {
		t.Fatalf("human exit = %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "auth") || !strings.Contains(stdout, "orders") {
		t.Fatalf("human output missing services: %q", stdout)
	}
	if !strings.Contains(stdout, "Workspace mesh") {
		t.Fatalf("human output missing workspace header: %q", stdout)
	}
}

func TestCLIBuildAll_RequiresWorkspace(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	code, _, stderr := runPhelixIn(t, home, dir, "build", "--all")
	if code != ExitNotFound {
		t.Fatalf("exit = %d, want %d (NOT_FOUND); stderr=%q", code, ExitNotFound, stderr)
	}
	if !strings.Contains(stderr, "no workspace found") {
		t.Fatalf("stderr = %q, want a no-workspace hint", stderr)
	}
}

func TestCLIBuildAll_RejectsNameWithAll(t *testing.T) {
	home, wsDir := newCLIWorkspace(t, true)
	code, _, stderr := runPhelixIn(t, home, wsDir, "build", "--all", "auth")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (USAGE); stderr=%q", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "--all cannot be combined") {
		t.Fatalf("stderr = %q, want a conflict message", stderr)
	}
}

func TestCLIDeployAll_EnforcedAuthorizationLeavesVersionsUnchanged(t *testing.T) {
	home, wsDir := newCLIWorkspace(t, true)
	phelixDir := filepath.Join(home, ".phelix")
	authzDir := filepath.Join(phelixDir, "authz")
	if err := os.MkdirAll(authzDir, 0o700); err != nil {
		t.Fatalf("mkdir authz: %v", err)
	}
	policy := `{"schema_version":"1","mode":"enforced","rules":[]}`
	if err := os.WriteFile(filepath.Join(authzDir, "policy.json"), []byte(policy), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	versionsPath := filepath.Join(phelixDir, "apps", "auth", "versions.json")
	before, err := os.ReadFile(versionsPath)
	if err != nil {
		t.Fatalf("read versions before deploy: %v", err)
	}

	code, _, stderr := runPhelixIn(t, home, wsDir, "deploy", "--all")
	if code != ExitPermission {
		t.Fatalf("exit = %d, want %d (PERMISSION); stderr=%q", code, ExitPermission, stderr)
	}
	if !strings.Contains(stderr, "plan") {
		t.Fatalf("stderr = %q, want a plan-based denial", stderr)
	}

	after, err := os.ReadFile(versionsPath)
	if err != nil {
		t.Fatalf("read versions after deploy: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("versions changed under a denied deploy:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestCLIDeployAll_UnregisteredServiceFailsClosed(t *testing.T) {
	home, wsDir := newCLIWorkspace(t, false)
	code, _, stderr := runPhelixIn(t, home, wsDir, "deploy", "--all")
	if code != ExitValidation {
		t.Fatalf("exit = %d, want %d (VALIDATION); stderr=%q", code, ExitValidation, stderr)
	}
	if !strings.Contains(stderr, "not registered") {
		t.Fatalf("stderr = %q, want a not-registered failure", stderr)
	}
}

func TestCLIWorkspaceLock_ContendedOperationFailsFast(t *testing.T) {
	home, wsDir := newCLIWorkspace(t, false)
	t.Setenv("PHELIX_DATA_DIR", filepath.Join(home, ".phelix"))
	release, err := workspace.AcquireWorkspaceLock("mesh", "deploy")
	if err != nil {
		t.Fatalf("AcquireWorkspaceLock in test process: %v", err)
	}
	defer release()

	code, _, stderr := runPhelixIn(t, home, wsDir, "build", "--all")
	if code != ExitDeploy {
		t.Fatalf("exit = %d, want %d (DEPLOY); stderr=%q", code, ExitDeploy, stderr)
	}
	if !strings.Contains(stderr, "WORKSPACE_LOCKED") {
		t.Fatalf("stderr = %q, want WORKSPACE_LOCKED", stderr)
	}
}

// Classic deploys keep no deploy.json, so the status view has to fall back to
// the reconciled app record. It used to report "not deployed" for an app that
// was in fact running, which also hid the version behind a deployed check.
func TestCLIStatusWorkspace_ClassicAppUsesAppRecord(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	wsDir := filepath.Join(root, "mesh")
	for _, dir := range []string{home, wsDir, filepath.Join(wsDir, "api")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	manifest := "services:\n  api: { path: ./api, port: 8090 }\n"
	if err := os.WriteFile(filepath.Join(wsDir, workspace.FileName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	phelixDir := filepath.Join(home, ".phelix")
	if err := os.MkdirAll(filepath.Join(phelixDir, "apps", "api"), 0o755); err != nil {
		t.Fatalf("mkdir app dir: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	apps := `{"a9":{"id":"a9","name":"api","pid":0,"status":"stopped","port":8090,` +
		`"directory":"` + filepath.Join(wsDir, "api") + `","language":"go",` +
		`"build_status":"success","created_at":"` + now + `","updated_at":"` + now + `"}}`
	if err := os.WriteFile(filepath.Join(phelixDir, "apps.json"), []byte(apps), 0o644); err != nil {
		t.Fatalf("write apps.json: %v", err)
	}
	versions := `{"versions":[{"version":4,"built_at":"` + now + `","size_bytes":1024,"is_current":true,"deployed_at":"` + now + `"}]}`
	if err := os.WriteFile(filepath.Join(phelixDir, "apps", "api", "versions.json"), []byte(versions), 0o644); err != nil {
		t.Fatalf("write versions.json: %v", err)
	}

	code, stdout, stderr := runPhelixIn(t, home, wsDir, "status", "--workspace", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d; stderr=%q", code, stderr)
	}
	var envelope struct {
		Result struct {
			Services []struct {
				Name    string `json:"name"`
				Status  string `json:"status"`
				Version int    `json:"version"`
				Port    int    `json:"port"`
			} `json:"services"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\nstdout=%q", err, stdout)
	}
	if len(envelope.Result.Services) != 1 {
		t.Fatalf("services = %+v, want 1", envelope.Result.Services)
	}
	api := envelope.Result.Services[0]
	if api.Status != "stopped" || api.Version != 4 || api.Port != 8090 {
		t.Fatalf("classic api view = %+v, want the app record's status with v4 on 8090", api)
	}
}
