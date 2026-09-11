package app

import (
	"path/filepath"
	"regexp"
	"testing"
)

func TestGenerateAppIDUsesIndependentUUIDs(t *testing.T) {
	manager := &AppManager{Apps: make(map[string]*AppInfo)}
	first := manager.GenerateAppID()
	second := manager.GenerateAppID()

	if first == second {
		t.Fatalf("generated duplicate app IDs: %q", first)
	}
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuid.MatchString(first) || !uuid.MatchString(second) {
		t.Fatalf("app IDs must be version 4 UUIDs: %q, %q", first, second)
	}
}

func TestRuntimeDataDirHonorsExplicitOverride(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "phelix-data")
	t.Setenv("PHELIX_DATA_DIR", dir)
	if got := runtimeDataDir(); got != dir {
		t.Fatalf("runtimeDataDir() = %q, want %q", got, dir)
	}
}
