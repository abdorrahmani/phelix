package app

import (
	"crypto/rand"
	"errors"
	"path/filepath"
	"regexp"
	"testing"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy unavailable")
}

func TestGenerateAppIDUsesIndependentUUIDs(t *testing.T) {
	manager := &AppManager{Apps: make(map[string]*AppInfo)}
	first, err := manager.GenerateAppID()
	if err != nil {
		t.Fatalf("GenerateAppID: %v", err)
	}
	second, err := manager.GenerateAppID()
	if err != nil {
		t.Fatalf("GenerateAppID: %v", err)
	}

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

func TestGenerateAppIDReportsEntropyFailure(t *testing.T) {
	original := rand.Reader
	rand.Reader = errReader{}
	defer func() { rand.Reader = original }()

	manager := &AppManager{Apps: make(map[string]*AppInfo)}
	if id, err := manager.GenerateAppID(); err == nil || id != "" {
		t.Fatalf("GenerateAppID = (%q, %v), want an error and no ID", id, err)
	}
}
