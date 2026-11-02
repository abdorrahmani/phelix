package logs

import (
	"os"
	"path/filepath"
	"testing"
)

// A worker replica writes its own file, so an explicit target path must be
// read instead of the ID-derived app file, and the replica must survive onto
// the entry.
func TestCollectAppLogs_ExplicitPathCarriesReplica(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())

	dir := t.TempDir()
	path := filepath.Join(dir, "deploy_binary_replica-3.log")
	if err := os.WriteFile(path, []byte("worker line\n"), 0o644); err != nil {
		t.Fatalf("write replica log: %v", err)
	}

	entries, err := CollectAppLogs([]AppLogTarget{{
		ID:      "app-worker",
		Name:    "wk",
		Path:    path,
		Replica: "3",
	}}, "srv-1")
	if err != nil {
		t.Fatalf("CollectAppLogs: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want one", entries)
	}
	if entries[0].Replica != "3" {
		t.Fatalf("Replica = %q, want 3", entries[0].Replica)
	}
	if entries[0].AppID != "app-worker" || entries[0].App != "wk" {
		t.Fatalf("entry identity = %+v", entries[0])
	}
}
