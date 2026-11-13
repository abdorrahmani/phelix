package project

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func boolPtr(v bool) *bool { return &v }

func TestHooksConfigValidateAcceptsMinimalHook(t *testing.T) {
	h := &HooksConfig{PreDeploy: &HookConfig{Command: []string{"./bin/migrate", "up"}}}
	if err := h.Validate(""); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestHooksConfigValidateRejects(t *testing.T) {
	cases := map[string]*HooksConfig{
		"missing command":      {PreBuild: &HookConfig{}},
		"blank executable":     {PreBuild: &HookConfig{Command: []string{" "}}},
		"nul byte":             {PreBuild: &HookConfig{Command: []string{"make", "a\x00b"}}},
		"bad timeout":          {PreBuild: &HookConfig{Command: []string{"make"}, Timeout: "soon"}},
		"negative timeout":     {PreBuild: &HookConfig{Command: []string{"make"}, Timeout: "-1s"}},
		"timeout too long":     {PreBuild: &HookConfig{Command: []string{"make"}, Timeout: "31m"}},
		"retries too high":     {PreBuild: &HookConfig{Command: []string{"make"}, Retries: MaxHookRetries + 1}},
		"negative retries":     {PreBuild: &HookConfig{Command: []string{"make"}, Retries: -1}},
		"bad retry interval":   {PreBuild: &HookConfig{Command: []string{"make"}, RetryInterval: "later"}},
		"absolute working dir": {PreBuild: &HookConfig{Command: []string{"make"}, WorkingDir: "/tmp"}},
		"escaping working dir": {PreBuild: &HookConfig{Command: []string{"make"}, WorkingDir: "../outside"}},
		"bad env name":         {PreBuild: &HookConfig{Command: []string{"make"}, Env: map[string]string{"1BAD": "x"}}},
		"reserved env name":    {PreBuild: &HookConfig{Command: []string{"make"}, Env: map[string]string{"PHELIX_HOOK": "x"}}},
		"critical post_deploy": {PostDeploy: &HookConfig{Command: []string{"make"}, ContinueOnError: boolPtr(false)}},
		"critical post_rollback": {
			PostRollback: &HookConfig{Command: []string{"make"}, ContinueOnError: boolPtr(false)},
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			err := h.Validate("")
			if err == nil {
				t.Fatal("Validate accepted an invalid hook")
			}
			if !phelixerr.IsCode(err, phelixerr.CodeConfiguration) {
				t.Fatalf("error code = %s, want CONFIGURATION_ERROR", phelixerr.CodeOf(err))
			}
		})
	}
}

func TestHooksConfigValidateConfinesWorkingDirToProject(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &HooksConfig{PreDeploy: &HookConfig{Command: []string{"make"}, WorkingDir: "migrations"}}
	if err := h.Validate(root); err != nil {
		t.Fatalf("Validate inside project: %v", err)
	}
	h.PreDeploy.WorkingDir = filepath.Join("migrations", "..", "..", "escape")
	if err := h.Validate(root); err == nil {
		t.Fatal("Validate accepted a working_dir escaping the project")
	}
}

func TestHooksConfigDefaults(t *testing.T) {
	cfg := &HookConfig{Command: []string{"make"}}
	if got := cfg.TimeoutDuration(); got != DefaultHookTimeout {
		t.Errorf("timeout = %s, want %s", got, DefaultHookTimeout)
	}
	if got := cfg.RetryIntervalDuration(); got != DefaultHookRetryInterval {
		t.Errorf("retry interval = %s, want %s", got, DefaultHookRetryInterval)
	}
	for _, name := range []string{HookPreBuild, HookPreDeploy, HookPostStart, HookPreTraffic} {
		if cfg.ContinuesOnError(name) {
			t.Errorf("%s must be critical by default", name)
		}
	}
	for _, name := range []string{HookPostDeploy, HookPostRollback, HookPreStop} {
		if !cfg.ContinuesOnError(name) {
			t.Errorf("%s must continue on error by default", name)
		}
	}
	critical := &HookConfig{Command: []string{"make"}, ContinueOnError: boolPtr(false)}
	if critical.ContinuesOnError(HookPostDeploy) {
		t.Error("explicit continue_on_error must win")
	}
	if (&HookConfig{Command: []string{"make"}, Timeout: "5s"}).TimeoutDuration() != 5*time.Second {
		t.Error("explicit timeout must win")
	}
}

func TestHooksConfigEnabledAndOrdering(t *testing.T) {
	h := &HooksConfig{
		PostDeploy: &HookConfig{Command: []string{"notify"}},
		PreBuild:   &HookConfig{Command: []string{"make"}, Enabled: boolPtr(false)},
	}
	if h.Enabled(HookPreBuild) {
		t.Error("disabled hook reported enabled")
	}
	if !h.Enabled(HookPostDeploy) {
		t.Error("hook without enabled key must default to enabled")
	}
	if h.Enabled(HookPostStart) {
		t.Error("absent hook reported enabled")
	}
	got := h.Configured()
	if len(got) != 2 || got[0] != HookPreBuild || got[1] != HookPostDeploy {
		t.Errorf("Configured() = %v, want [pre_build post_deploy]", got)
	}
	if (*HooksConfig)(nil).Enabled(HookPreBuild) || (*HooksConfig)(nil).Configured() != nil {
		t.Error("nil HooksConfig must be inert")
	}
}

func TestLoadRejectsInvalidHook(t *testing.T) {
	dir := t.TempDir()
	body := "name: app\nport: 8080\nhooks:\n  pre_build:\n    command: []\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a hook without a command")
	}
	if !phelixerr.IsCode(err, phelixerr.CodeConfiguration) {
		t.Fatalf("error code = %s, want CONFIGURATION_ERROR", phelixerr.CodeOf(err))
	}
}

func TestLoadWithoutHooksKeepsWorking(t *testing.T) {
	dir := t.TempDir()
	body := "name: app\nport: 8080\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Hooks != nil {
		t.Fatalf("Hooks = %+v, want nil", cfg.Hooks)
	}
}
