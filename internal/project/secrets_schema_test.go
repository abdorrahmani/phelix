package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func loadSecretsYAML(t *testing.T, yaml string) (*SecretsConfig, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		return nil, err
	}
	return cfg.Secrets, nil
}

func TestSecretsConfigLoad(t *testing.T) {
	t.Run("no secrets section leaves existing projects unaffected", func(t *testing.T) {
		cfg, err := loadSecretsYAML(t, "name: api\nport: 8080\n")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg != nil {
			t.Fatalf("expected nil secrets config, got %+v", cfg)
		}
	})

	t.Run("empty section loads", func(t *testing.T) {
		cfg, err := loadSecretsYAML(t, "name: api\nsecrets: {}\n")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg == nil || len(cfg.Imports) != 0 {
			t.Fatalf("config = %+v", cfg)
		}
	})

	t.Run("valid imports load", func(t *testing.T) {
		yaml := "name: api\nsecrets:\n  imports:\n    - secret: database_url\n      env: DATABASE_URL\n    - secret: database_url\n      env: DB_DSN\n"
		cfg, err := loadSecretsYAML(t, yaml)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(cfg.Imports) != 2 {
			t.Fatalf("imports = %+v", cfg.Imports)
		}
		if cfg.Imports[0].Secret != "database_url" || cfg.Imports[0].Env != "DATABASE_URL" {
			t.Fatalf("first import = %+v", cfg.Imports[0])
		}
	})

	invalid := map[string]string{
		"missing secret":          "secrets:\n  imports:\n    - env: DATABASE_URL\n",
		"missing env":             "secrets:\n  imports:\n    - secret: database_url\n",
		"uppercase secret":        "secrets:\n  imports:\n    - secret: DatabaseURL\n      env: DATABASE_URL\n",
		"secret with slash":       "secrets:\n  imports:\n    - secret: a/b\n      env: DATABASE_URL\n",
		"env starting with digit": "secrets:\n  imports:\n    - secret: db\n      env: 1DATABASE\n",
		"env with hyphen":         "secrets:\n  imports:\n    - secret: db\n      env: DATABASE-URL\n",
		"reserved PORT":           "secrets:\n  imports:\n    - secret: port\n      env: PORT\n",
		"duplicate env":           "secrets:\n  imports:\n    - secret: db\n      env: DATABASE_URL\n    - secret: other\n      env: DATABASE_URL\n",
	}

	for name, body := range invalid {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := loadSecretsYAML(t, "name: api\n"+body)
			if err == nil {
				t.Fatalf("expected rejection for %s", name)
			}
			if phelixerr.CodeOf(err) != phelixerr.CodeConfiguration {
				t.Fatalf("expected configuration error, got %v", err)
			}
			if !strings.Contains(err.Error(), "secrets.imports[") {
				t.Fatalf("error should name the yaml path, got %v", err)
			}
		})
	}
}

func TestSecretsStrictLint(t *testing.T) {
	dir := t.TempDir()
	body := "name: api\nsecrets:\n  imports:\n    - secret: database_url\n      env: DATABASE_URL\n      typo_key: x\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadStrict(dir)
	if err == nil {
		t.Fatal("strict load must reject unknown keys inside secrets.imports")
	}
	if !strings.Contains(err.Error(), "secrets.imports[].typo_key") {
		t.Fatalf("error should name the unknown key path, got %v", err)
	}
}

func TestSecretsValidateExportedMatchesLoad(t *testing.T) {
	s := &SecretsConfig{Imports: []SecretImport{{Secret: "db", Env: "PORT"}}}
	if err := s.Validate(); err == nil {
		t.Fatal("Validate must reject reserved names like the loader does")
	}
	var nilSecrets *SecretsConfig
	if err := nilSecrets.Validate(); err != nil {
		t.Fatalf("nil section must validate, got %v", err)
	}
}

func TestIsReservedEnvName(t *testing.T) {
	if !IsReservedEnvName("PORT") {
		t.Fatal("PORT must be reserved")
	}
	if IsReservedEnvName("DATABASE_URL") {
		t.Fatal("DATABASE_URL must not be reserved")
	}
}
