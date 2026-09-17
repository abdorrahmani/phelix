package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/abdorrahmani/phelix/internal/resources"
)

// withStateFile points the package state file at dir/apps.json (the runtime
// paths are fixed at init; tests redirect via PHELIX_DATA_DIR before the
// package loads — here we set the file variables directly since LoadResources
// reads them).
func withStateFile(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PHELIX_DATA_DIR", dir)
	origState := stateFile
	origLogDir := logDir
	stateFile = filepath.Join(dir, "apps.json")
	logDir = filepath.Join(dir, "logs")
	t.Cleanup(func() {
		stateFile = origState
		logDir = origLogDir
	})
}

func writeState(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "apps.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadResources_MissingStateFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	withStateFile(t, dir)

	if _, err := LoadResources("ghost"); err == nil {
		t.Fatal("missing state file must fail closed")
	}
}

func TestLoadResources_MissingAppFailsClosed(t *testing.T) {
	dir := t.TempDir()
	withStateFile(t, dir)
	writeState(t, dir, `{"id1":{"id":"id1","name":"web","resources":{"cpu":"500m","memory":"256Mi"}}}`)

	if _, err := LoadResources("other"); err == nil {
		t.Fatal("missing app must fail closed")
	}
}

func TestLoadResources_LegacyAppReturnsZero(t *testing.T) {
	dir := t.TempDir()
	withStateFile(t, dir)
	writeState(t, dir, `{"id1":{"id":"id1","name":"web"}}`)

	cfg, err := LoadResources("web")
	if err != nil || cfg != (resources.Config{}) {
		t.Fatalf("legacy app must remain unlimited: %+v, %v", cfg, err)
	}
}

func TestLoadResources_ExactValuesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	withStateFile(t, dir)
	writeState(t, dir, `{"id1":{"id":"id1","name":"web","pid":12,"resources":{"cpu":"1.5","memory":"1Gi"}}}`)

	cfg, err := LoadResources("web")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CPU != "1.5" || cfg.Memory != "1Gi" {
		t.Fatalf("want cpu=1.5 memory=1Gi, got %+v", cfg)
	}
}

func TestLoadResources_PartialDecodeLeavesUnrelatedFieldsUntouched(t *testing.T) {
	dir := t.TempDir()
	withStateFile(t, dir)
	// pid is a number, watching a bool, process a nested object: none may
	// disturb the decode of name/resources.
	writeState(t, dir, `{"id1":{"id":"id1","name":"web","pid":12,"watching":true,"resources":{"cpu":"2","memory":"512Mi"}}}`)

	cfg, err := LoadResources("web")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CPU != "2" || cfg.Memory != "512Mi" {
		t.Fatalf("want cpu=2 memory=512Mi, got %+v", cfg)
	}
}

func TestLoadResources_MalformedStateFailsClosed(t *testing.T) {
	dir := t.TempDir()
	withStateFile(t, dir)
	writeState(t, dir, `{not json`)

	_, err := LoadResources("web")
	if err == nil {
		t.Fatal("malformed state file must fail closed")
	}
}

func TestLoadResources_ReadErrorFailsClosed(t *testing.T) {
	dir := t.TempDir()
	withStateFile(t, dir)
	writeState(t, dir, `{}`)
	// A directory at the state path makes ReadFile fail with EISDIR.
	if err := os.Remove(filepath.Join(dir, "apps.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "apps.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadResources("web"); err == nil {
		t.Fatal("unreadable state file must fail closed")
	}
}

func TestLoadResources_DoesNotModifyStateFile(t *testing.T) {
	dir := t.TempDir()
	withStateFile(t, dir)
	content := `{"id1":{"id":"id1","name":"web","status":"running","pid":9999,"resources":{"cpu":"250m"}}}`
	writeState(t, dir, `{"id1":{"id":"id1","name":"web","status":"running","pid":9999,"resources":{"cpu":"250m"}}}`)

	if _, err := LoadResources("web"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != content {
		t.Fatalf("state file was modified:\nwant %s\ngot  %s", content, string(raw))
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("state file no longer valid JSON: %v", err)
	}
}
