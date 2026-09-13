package cmd

import (
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// FormatWatching renders the flag as a human-readable state, never a raw
// boolean.
func TestFormatWatching(t *testing.T) {
	if got := FormatWatching(true); !strings.Contains(got, "enabled") {
		t.Fatalf("FormatWatching(true) = %q, want \"enabled\"", got)
	}
	if got := FormatWatching(false); !strings.Contains(got, "disabled") {
		t.Fatalf("FormatWatching(false) = %q, want \"disabled\"", got)
	}
}

func TestPopulateTable_ShowsWatchingColumn(t *testing.T) {
	out := captureStdout(t, func() {
		table := createTable()
		populateTable(table, []app.AppListItem{
			{ID: "203", Name: "watched", Status: "stopped", Watching: true},
			{ID: "204", Name: "unwatched", Status: "stopped", Watching: false},
		}, map[string]proxy.AppStatus{})
		table.Render()
	})

	for _, want := range []string{"WATCHING", "enabled", "disabled", "watched", "unwatched"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestPopulateStatusTable_ShowsWatchingColumn(t *testing.T) {
	out := captureStdout(t, func() {
		table := createStatusTable()
		populateStatusTable(table, app.AppStatus{ID: "203", Name: "demo", Status: "stopped", Watching: true})
		table.Render()
	})

	for _, want := range []string{"WATCHING", "enabled"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q\noutput:\n%s", want, out)
		}
	}

	out = captureStdout(t, func() {
		table := createStatusTable()
		populateStatusTable(table, app.AppStatus{ID: "203", Name: "demo", Status: "stopped"})
		table.Render()
	})
	if !strings.Contains(out, "disabled") {
		t.Errorf("status output for an unwatched app missing \"disabled\"\noutput:\n%s", out)
	}
}
