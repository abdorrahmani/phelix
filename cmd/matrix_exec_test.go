package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/matrix"
)

// A completed combination must reach disk through the journal, leaving the
// run document untouched until the session finalizes.
func TestMatrixSession_ResultIsJournaledNotRewritten(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dataDir)

	prof := &matrix.Profile{Lang: builder.Go, Versions: []string{"1.0"}, Platforms: []string{"linux/amd64"}}
	run := matrix.NewRun(matrix.NewUniqueRunID(time.Now()), "app", dataDir, prof, time.Now())
	run.InitCombinations([]matrix.Combination{
		{Lang: builder.Go, Version: "1.0", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
	})
	if err := matrix.SaveRun(run); err != nil {
		t.Fatal(err)
	}

	docPath := filepath.Join(dataDir, "matrix", "runs", string(run.ID)+".json")
	before, err := os.Stat(docPath)
	if err != nil {
		t.Fatal(err)
	}

	// Drive the combination to completion through the executor, journalling
	// each result exactly as the session core does.
	journalDir, err := matrix.RunsDir()
	if err != nil {
		t.Fatal(err)
	}
	results := matrix.Execute(
		&matrix.MatrixPlan{Lang: builder.Go, Combinations: run.IncompleteCombinations()},
		func(_ context.Context, c matrix.Combination) *matrix.Result {
			return &matrix.Result{Combination: c, Status: "success", SHA256: "abc"}
		},
		matrix.ExecutorConfig{
			Concurrency: 1,
			OnResult: func(res matrix.Result) {
				run.RecordResult(res)
				entry, ok := run.CombinationEntry(res.Combination.ID())
				if !ok {
					t.Errorf("combination %s missing", res.Combination.ID())
					return
				}
				if err := matrix.AppendJournal(journalDir, run.ID, entry); err != nil {
					t.Errorf("AppendJournal: %v", err)
				}
			},
		},
	)
	if len(results) != 1 || results[0].Status != "success" {
		t.Fatalf("expected one success, got %+v", results)
	}

	// The run document must not have been touched.
	after, err := os.Stat(docPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the run document was rewritten for a result; it must be journaled instead")
	}
	if after.Size() != before.Size() {
		t.Fatalf("run document size changed: %d -> %d", before.Size(), after.Size())
	}

	// The result must still be readable, from the journal.
	loaded, err := matrix.LoadRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Succeeded != 1 {
		t.Fatalf("journaled result not visible on load: succeeded = %d", loaded.Succeeded)
	}
	if got := loaded.Combinations[0].Status; got != "success" {
		t.Fatalf("combination status after replay = %q, want success", got)
	}
}
