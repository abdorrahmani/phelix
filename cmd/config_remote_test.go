package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/project"
)

func TestAppConfigAgentBridge(t *testing.T) {
	dir := os.Getenv("PHELIX_CONFIG_TEST_DIR")
	if dir == "" {
		return
	}
	var task phelixgrpc.AppConfigTask
	if err := json.NewDecoder(os.Stdin).Decode(&task); err != nil {
		t.Fatal(err)
	}
	result := executeManagedAppConfigTask(&app.AppInfo{Name: task.AppName, Directory: dir, Port: 8080}, &task)
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteConfigCommandsValidateAndSave(t *testing.T) {
	dir := t.TempDir()
	task := &phelixgrpc.AppConfigTask{RequestID: "request", AppName: "demo", Command: phelixgrpc.AppConfigCommandRead}
	missing := executeAppConfigTask(dir, task)
	if missing.Status != "success" || missing.Exists || missing.Schema == "" {
		t.Fatalf("missing: %+v", missing)
	}
	task.Command, task.Content = phelixgrpc.AppConfigCommandValidate, "deploy: {strategy: invalid}\n"
	invalid := executeAppConfigTask(dir, task)
	if invalid.Status != "success" || invalid.Valid || len(invalid.Problems) == 0 || invalid.Problems[0].Path != "deploy.strategy" || invalid.Problems[0].Line != 1 {
		t.Fatalf("invalid: %+v", invalid)
	}
	if _, err := os.Stat(filepath.Join(dir, project.FileName)); !os.IsNotExist(err) {
		t.Fatal("validation modified configuration")
	}
	task.Command, task.Content = phelixgrpc.AppConfigCommandCreate, "# retained\nname: demo\nport: 8080\nunknown: {nested: [true, 3]}\n"
	created := executeAppConfigTask(dir, task)
	if created.Status != "success" || !created.Valid || !created.Changed || !created.Exists || created.Revision == "" || created.UpdatedAt == "" {
		t.Fatalf("create: %+v", created)
	}
	task.Command, task.ExpectedRevision = phelixgrpc.AppConfigCommandSave, created.Revision
	identical := executeAppConfigTask(dir, task)
	if identical.Status != "success" || identical.Changed || identical.UpdatedAt != created.UpdatedAt {
		t.Fatalf("identical: %+v", identical)
	}
	task.Content = "name: changed\nport: 9090\n"
	saved := executeAppConfigTask(dir, task)
	if saved.Status != "success" || !saved.Changed || saved.Revision == created.Revision {
		t.Fatalf("save: %+v", saved)
	}
	task.Content = "name: stale\n"
	conflict := executeAppConfigTask(dir, task)
	if conflict.Status != "error" || conflict.ErrorCode != "IDEMPOTENCY_CONFLICT" || conflict.Content != saved.Content || conflict.Revision != saved.Revision {
		t.Fatalf("conflict: %+v", conflict)
	}
	task.Command = phelixgrpc.AppConfigCommandRead
	loaded := executeAppConfigTask(dir, task)
	if loaded.Status != "success" || !loaded.Valid || loaded.Content != saved.Content {
		t.Fatalf("read: %+v", loaded)
	}
	cfg, err := project.Load(dir)
	if err != nil || cfg.Port != 9090 {
		t.Fatalf("CLI load cfg=%+v err=%v", cfg, err)
	}
}

func TestConfigDiagnosticsLocateInvalidValues(t *testing.T) {
	for _, raw := range []string{"port: 70000\n", "deploy:\n  strategy: bad\n", "unknown:\n  key: 1\n  key: 2\n"} {
		_, err := project.ValidateContent(t.TempDir(), []byte(raw))
		if err == nil {
			t.Fatal("invalid source accepted")
		}
		problems := configProblems([]byte(raw), err)
		if problems[0].Line == 0 {
			t.Fatalf("missing diagnostic line: %s", fmt.Sprint(problems))
		}
	}
}

func TestRemoteConfigTaskHonorsPolicies(t *testing.T) {
	dir := t.TempDir()
	info := &app.AppInfo{Name: "demo", Directory: dir, Port: 8080}
	task := &phelixgrpc.AppConfigTask{RequestID: "policy", AppName: "demo", Command: phelixgrpc.AppConfigCommandRead, WriteDeniedPaths: []project.ConfigDeniedPath{{Path: "health", Message: "health subscription required"}}}
	missing := executeManagedAppConfigTask(info, task)
	if missing.Status != "success" || !missing.PoliciesApplied || missing.InitialContent == "" {
		t.Fatalf("missing policy response: %+v", missing)
	}
	task.Command, task.Content = phelixgrpc.AppConfigCommandCreate, missing.InitialContent
	created := executeManagedAppConfigTask(info, task)
	if created.Status != "success" || !created.PoliciesApplied {
		t.Fatalf("default creation: %+v", created)
	}
	task.Command, task.ExpectedRevision = phelixgrpc.AppConfigCommandSave, created.Revision
	task.Content = strings.Replace(created.Content, "/health", "/ready", 1)
	denied := executeManagedAppConfigTask(info, task)
	if denied.Status != "error" || denied.ErrorCode != "PERMISSION_DENIED" || denied.Content != "" {
		t.Fatalf("protected change accepted: %+v", denied)
	}
	secretSource := "name: demo\nhooks: {pre_build: {command: [./build], env: {TOKEN: never_return}}}\n"
	if err := os.WriteFile(filepath.Join(dir, project.FileName), []byte(secretSource), 0o600); err != nil {
		t.Fatal(err)
	}
	task.Command = phelixgrpc.AppConfigCommandRead
	task.ReadDeniedPaths = []project.ConfigDeniedPath{{Path: "hooks.*.env", Message: "environment view permission required"}}
	protected := executeManagedAppConfigTask(info, task)
	if protected.Status != "error" || protected.ErrorCode != "PERMISSION_DENIED" || protected.Content != "" || strings.Contains(protected.Error, "never_return") {
		t.Fatalf("protected file exposed: %+v", protected)
	}
	task.Command = phelixgrpc.AppConfigCommandValidate
	task.Content = "port: invalid\n"
	validation := executeManagedAppConfigTask(info, task)
	if validation.Status != "success" || validation.Valid || !validation.PoliciesApplied {
		t.Fatalf("invalid validation policy acknowledgement: %+v", validation)
	}
}

func TestRemoteConfigMissingDirectoryHasReadableError(t *testing.T) {
	result := executeManagedAppConfigTask(&app.AppInfo{Name: "demo"}, &phelixgrpc.AppConfigTask{RequestID: "missing", Command: phelixgrpc.AppConfigCommandRead})
	if result.Status != "error" || !strings.Contains(result.Error, "could not be accessed safely") {
		t.Fatalf("missing directory error: %+v", result)
	}
}
