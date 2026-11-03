package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadStrictRejectsUnknownKeyWithLocation(t *testing.T) {
	dir := writeConfig(t, "name: api\ndeploy:\n  repilcas: 3\n")

	_, err := LoadStrict(dir)
	if !phelixerr.IsCode(err, phelixerr.CodeConfiguration) {
		t.Fatalf("strict load code = %v, want CONFIGURATION_ERROR", phelixerr.CodeOf(err))
	}
	msg := err.Error()
	if !strings.Contains(msg, "deploy.repilcas") {
		t.Errorf("error must name the dotted key path, got %q", msg)
	}
	if !strings.Contains(msg, FileName+":3:") {
		t.Errorf("error must name the source line, got %q", msg)
	}
}

func TestLoadPermissiveWarnsAndStillLoadsUnknownKey(t *testing.T) {
	dir := writeConfig(t, "name: api\ndeploy:\n  repilcas: 3\n")

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("permissive load must accept unknown keys during the transition: %v", err)
	}
	if cfg.Name != "api" {
		t.Fatalf("cfg = %+v, want name api", cfg)
	}
}

func TestLoadStrictRejectsMisplacedWorkerField(t *testing.T) {
	dir := writeConfig(t, "name: api\ndeploy:\n  restart:\n    policy: always\n")

	_, err := LoadStrict(dir)
	if err == nil || !strings.Contains(err.Error(), "deploy.restart") {
		t.Fatalf("misplaced worker field must fail with its path, got %v", err)
	}
}

func TestLoadStrictRejectsMultipleDocuments(t *testing.T) {
	dir := writeConfig(t, "name: api\nport: 3000\n---\nname: other\nport: 4000\n")

	_, err := LoadStrict(dir)
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("multiple documents must fail, got %v", err)
	}
}

func TestLoadStrictRejectsDuplicateKeys(t *testing.T) {
	dir := writeConfig(t, "name: api\nname: other\n")

	_, err := LoadStrict(dir)
	if err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Fatalf("duplicate key must fail, got %v", err)
	}
}

func TestLoadStrictRejectsContradictoryWorkload(t *testing.T) {
	dir := writeConfig(t, "name: worker\ntype: worker\ndeploy:\n  runtime: docker\n  strategy: rolling\n")

	_, err := LoadStrict(dir)
	if err == nil || !strings.Contains(err.Error(), "not supported for type: worker") {
		t.Fatalf("worker with docker runtime must fail, got %v", err)
	}
}

func TestLoadStrictAcceptsRepositoryExamples(t *testing.T) {
	err := filepath.WalkDir("../../examples", func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || d.Name() != FileName {
			return err
		}
		if _, err := LoadStrict(filepath.Dir(path)); err != nil {
			t.Errorf("%s does not load strictly: %v", path, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk examples: %v", err)
	}
}
