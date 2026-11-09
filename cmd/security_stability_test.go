package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
)

// TestSecurityPeriodicPassesEmitOneWarning verifies that a registry full of
// stale project directories produces one summary warning per pass instead of
// one warning per application, which would flood the log transport.
func TestSecurityPeriodicPassesEmitOneWarning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, ".phelix")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	registry := map[string]map[string]any{}
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("app-%03d", i)
		registry[id] = map[string]any{
			"id":        id,
			"name":      id,
			"status":    "stopped",
			"directory": filepath.Join(home, "missing", id),
		}
	}
	blob, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "apps.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.SetStateDirForTesting(dataDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Manager.LoadState() })

	var messages []string
	original := securityWarningFile
	securityWarningFile = func(component, format string, args ...any) {
		messages = append(messages, fmt.Sprintf(format, args...))
	}
	defer func() { securityWarningFile = original }()

	securitySnapshotsForConsentingApps()
	listSecurityScheduledApps()

	if len(messages) != 2 {
		t.Fatalf("warnings = %d, want 2: %v", len(messages), messages)
	}
	for _, message := range messages {
		if !strings.Contains(message, "100 application(s)") {
			t.Fatalf("summary missing the skipped count: %q", message)
		}
	}
}
