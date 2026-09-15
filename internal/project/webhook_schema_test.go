package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func loadWebhookYAML(t *testing.T, yaml string) (*WebhookConfig, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		return nil, err
	}
	return cfg.Webhook, nil
}

func TestWebhookConfigLoad(t *testing.T) {
	t.Run("no webhook section leaves existing projects unaffected", func(t *testing.T) {
		cfg, err := loadWebhookYAML(t, "name: api\nport: 8080\n")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg != nil {
			t.Fatalf("expected nil webhook config, got %+v", cfg)
		}
	})

	t.Run("defaults to disabled", func(t *testing.T) {
		cfg, err := loadWebhookYAML(t, "name: api\nwebhook:\n  branch: main\n  secret_env: PHELIX_WEBHOOK_SECRET\n")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg == nil || cfg.Enabled {
			t.Fatalf("webhook without enabled must default to disabled, got %+v", cfg)
		}
	})

	t.Run("valid enabled section loads", func(t *testing.T) {
		cfg, err := loadWebhookYAML(t, "name: api\nwebhook:\n  enabled: true\n  branch: main\n  secret_env: PHELIX_WEBHOOK_SECRET\n")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !cfg.Enabled || cfg.Branch != "main" || cfg.SecretEnv != "PHELIX_WEBHOOK_SECRET" {
			t.Fatalf("config = %+v", cfg)
		}
	})

	invalid := map[string]string{
		"enabled without branch":     "webhook:\n  enabled: true\n  secret_env: PHELIX_WEBHOOK_SECRET\n",
		"enabled without secret_env": "webhook:\n  enabled: true\n  branch: main\n",
		"full ref as branch":         "webhook:\n  enabled: true\n  branch: refs/heads/main\n  secret_env: PHELIX_WEBHOOK_SECRET\n",
		"branch with ref traversal":  "webhook:\n  enabled: true\n  branch: 'a..b'\n  secret_env: PHELIX_WEBHOOK_SECRET\n",
		"branch with whitespace":     "webhook:\n  enabled: true\n  branch: 'main dev'\n  secret_env: PHELIX_WEBHOOK_SECRET\n",
		"invalid secret_env name":    "webhook:\n  enabled: true\n  branch: main\n  secret_env: '1BAD-NAME'\n",
		"empty secret_env name":      "webhook:\n  enabled: true\n  branch: main\n  secret_env: ''\n",
	}
	for name, yaml := range invalid {
		t.Run("invalid: "+name, func(t *testing.T) {
			_, err := loadWebhookYAML(t, "name: api\n"+yaml)
			if err == nil {
				t.Fatal("expected a configuration error")
			}
			if !phelixerr.IsCode(err, phelixerr.CodeConfiguration) {
				t.Fatalf("error code = %v, want CONFIGURATION_ERROR", phelixerr.CodeOf(err))
			}
			if !strings.Contains(err.Error(), "webhook.") {
				t.Fatalf("error must name the yaml path, got: %s", err)
			}
		})
	}

	t.Run("multi-segment branch is valid", func(t *testing.T) {
		cfg, err := loadWebhookYAML(t, "webhook:\n  enabled: true\n  branch: release/2.x\n  secret_env: PHELIX_WEBHOOK_SECRET\n")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.Branch != "release/2.x" {
			t.Fatalf("branch = %q", cfg.Branch)
		}
	})
}
