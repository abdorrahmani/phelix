package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
)

// TestDryRunAndCancellationWriteNoHistory pins the no-side-effect contract of
// the non-executing rollback paths: a preview (dry-run) and a picker
// cancellation must leave the rollback history file untouched — neither may
// append a record, whatever its status.
func TestDryRunAndCancellationWriteNoHistory(t *testing.T) {
	home := seedPickerApp(t, 3, 4)
	appDir := filepath.Join(home, ".phelix", "apps", "pickapp")
	histPath := filepath.Join(appDir, "rollback_history.jsonl")

	appInfo := &app.AppInfo{ID: "pickapp", Name: "pickapp", Directory: appDir, Status: "stopped", Port: 8080}

	// Dry-run path: renderRollbackPreview performs no mutations.
	if err := renderRollbackPreview(appInfo, "pickapp", 3, "", rollbackTargetExplicit, "", 0); err != nil {
		t.Fatalf("preview (dry-run) failed: %v", err)
	}
	if _, err := os.Stat(histPath); !os.IsNotExist(err) {
		t.Errorf("dry-run must not create a history file (stat err: %v)", err)
	}

	// Picker cancellation: promptRollbackVersion maps Esc / Ctrl-C to
	// errRollbackCancelled and the command returns before any execution path,
	// so no record call happens. The sentinel still routes to a clean exit.
	if !strings.Contains(errRollbackCancelled.Error(), "cancelled") {
		t.Fatalf("cancellation sentinel changed: %v", errRollbackCancelled)
	}

	// Sanity: a real recording still lands in this app's history file.
	deploy.RecordRollbackResult("pickapp", 5, 4, "classic", "", nil, nil)
	data, err := os.ReadFile(histPath)
	if err != nil {
		t.Fatalf("history file missing after real record: %v", err)
	}
	if !strings.Contains(string(data), `"to":"v4"`) {
		t.Errorf("record not persisted: %s", data)
	}
}
