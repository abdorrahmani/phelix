package cmd

import (
	"encoding/json"
	"fmt"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"os"
	"path/filepath"
	"runtime"
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

func TestConfigDiagnosticsBoundLargeMessages(t *testing.T) {
	err := phelixerr.New(phelixerr.CodeConfiguration, "webhook.secret_env: "+strings.Repeat("<", 512<<10))
	problems := configProblems(nil, err)
	if len(problems[0].Message) > 4099 || problems[0].Path != "webhook.secret_env" {
		t.Fatalf("oversized diagnostic: length=%d path=%s", len(problems[0].Message), problems[0].Path)
	}
	task := &phelixgrpc.AppConfigTask{RequestID: "bounded"}
	result := appConfigFailure(task, err)
	raw, encodeErr := json.Marshal(result)
	if encodeErr != nil || len(raw) > 64<<10 {
		t.Fatalf("oversized failure response length=%d err=%v", len(raw), encodeErr)
	}
}

func TestRemoteConfigSharedDirectoryRequiresOwner(t *testing.T) {
	dir := t.TempDir()
	source := []byte("name: shared\nport: 8080\n")
	if err := os.WriteFile(filepath.Join(dir, project.FileName), source, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := app.Manager.(*app.AppManager)
	manager.Lock.Lock()
	original := manager.Apps
	manager.Apps = map[string]*app.AppInfo{
		"first-id":  {ID: "first-id", Name: "first", Directory: dir, Port: 8080},
		"second-id": {ID: "second-id", Name: "second", Directory: dir, Port: 8080},
	}
	manager.Lock.Unlock()
	t.Cleanup(func() { manager.Lock.Lock(); manager.Apps = original; manager.Lock.Unlock() })
	info, err := resolveConfigAppInfo("first-id")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{phelixgrpc.AppConfigCommandRead, phelixgrpc.AppConfigCommandValidate, phelixgrpc.AppConfigCommandCreate, phelixgrpc.AppConfigCommandSave} {
		task := &phelixgrpc.AppConfigTask{RequestID: "shared", AppName: "first-id", Command: command, Content: "name: changed\n", ExpectedRevision: project.ContentRevision(source)}
		denied := executeManagedAppConfigTask(info, task)
		if denied.Status != "error" || denied.ErrorCode != "PERMISSION_DENIED" || denied.Content != "" {
			t.Fatalf("nonowner shared operation %s: %+v", command, denied)
		}
	}
	task := &phelixgrpc.AppConfigTask{RequestID: "owner", AppName: "first-id", Command: phelixgrpc.AppConfigCommandRead, OwnerAccess: true}
	allowed := executeManagedAppConfigTask(info, task)
	if allowed.Status != "success" || allowed.Content != string(source) {
		t.Fatalf("owner shared read: %+v", allowed)
	}
	after, err := os.ReadFile(filepath.Join(dir, project.FileName))
	if err != nil || string(after) != string(source) {
		t.Fatalf("denied operation changed shared file: %q %v", after, err)
	}
	ownerSave := &phelixgrpc.AppConfigTask{RequestID: "owner-save", AppName: "first-id", Command: phelixgrpc.AppConfigCommandSave, OwnerAccess: true, Content: "name: shared\nport: 9090\n", ExpectedRevision: project.ContentRevision(source)}
	if saved := executeManagedAppConfigTask(info, ownerSave); saved.Status != "success" || !saved.Changed {
		t.Fatalf("owner shared save: %+v", saved)
	}
	if runtime.GOOS != "windows" {
		alias := filepath.Join(t.TempDir(), "shared-alias")
		if err := os.Symlink(dir, alias); err != nil {
			t.Fatal(err)
		}
		manager.Lock.Lock()
		manager.Apps["second-id"].Directory = alias
		manager.Lock.Unlock()
		task.OwnerAccess = false
		if result := executeManagedAppConfigTask(info, task); result.Status != "error" || result.Content != "" {
			t.Fatalf("directory alias bypassed isolation: %+v", result)
		}
	}
	secondDir := t.TempDir()
	manager.Lock.Lock()
	manager.Apps["second-id"].Directory = secondDir
	manager.Lock.Unlock()
	task.OwnerAccess = false
	unique := executeManagedAppConfigTask(info, task)
	if unique.Status != "success" {
		t.Fatalf("unique directory denied: %+v", unique)
	}
	manager.Lock.Lock()
	manager.Apps["second-id"].Directory = filepath.Join(secondDir, "missing")
	manager.Lock.Unlock()
	if result := executeManagedAppConfigTask(info, task); result.Status != "success" {
		t.Fatalf("unavailable unrelated project blocked unique configuration: %+v", result)
	}

}

func TestRemoteConfigIdentityRejectsAmbiguousNames(t *testing.T) {
	firstDir, secondDir := t.TempDir(), t.TempDir()
	manager := app.Manager.(*app.AppManager)
	manager.Lock.Lock()
	original := manager.Apps
	manager.Apps = map[string]*app.AppInfo{
		"first-id":  {ID: "first-id", Name: "duplicate", Directory: firstDir},
		"second-id": {ID: "second-id", Name: "duplicate", Directory: secondDir},
	}
	manager.Lock.Unlock()
	t.Cleanup(func() { manager.Lock.Lock(); manager.Apps = original; manager.Lock.Unlock() })
	if info, err := resolveConfigAppInfo("duplicate"); err == nil || info != nil {
		t.Fatalf("ambiguous name selected an application: %+v %v", info, err)
	}
	if info, err := resolveConfigAppInfo("first-id"); err != nil || info.ID != "first-id" {
		t.Fatalf("stable application identity rejected: %+v %v", info, err)
	}
}
