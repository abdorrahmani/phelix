package matrix

import (
	"os"
	"strings"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// newTestRun writes a run with n distinct combinations and returns it.
// Each combination needs a distinct version so its ID is distinct.
func newTestRun(tb testing.TB, n int) *Run {
	tb.Helper()
	tb.Setenv("PHELIX_DATA_DIR", tb.TempDir())

	combos := make([]Combination, 0, n)
	for i := range n {
		combos = append(combos, Combination{
			Lang:     "go",
			Version:  versionForIndex(i),
			OS:       "linux",
			Arch:     "amd64",
			Platform: "linux/amd64",
		})
	}
	prof := &Profile{Lang: "go", Versions: []string{"1.0"}, Platforms: []string{"linux/amd64"}}
	r := NewRun(NewUniqueRunID(time.Now()), "app", "/tmp/proj", prof, time.Now())
	r.InitCombinations(combos)
	if err := SaveRun(r); err != nil {
		tb.Fatalf("SaveRun: %v", err)
	}
	return r
}

// versionForIndex yields a distinct version per index, so every combination
// gets a distinct ID.
func versionForIndex(i int) string {
	return "1." + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
}

func runDirOf(tb testing.TB, r *Run) string {
	tb.Helper()
	dir, err := RunsDir()
	if err != nil {
		tb.Fatal(err)
	}
	return dir
}

func findCombination(r *Run, id string) *RunCombination {
	for i := range r.Combinations {
		if r.Combinations[i].ID == id {
			return &r.Combinations[i]
		}
	}
	return nil
}

// A recorded entry must survive the round trip through disk and replay.
func TestJournal_RoundTrip(t *testing.T) {
	r := newTestRun(t, 5)
	dir := runDirOf(t, r)
	combos := r.IncompleteCombinations()

	for _, c := range combos {
		r.MarkRunning(c.ID(), 1, time.Now())
		if err := appendJournal(dir, r.ID, *findCombination(r, c.ID())); err != nil {
			t.Fatal(err)
		}
		r.RecordResult(Result{Combination: c, Status: "success",
			Artifact: "builds/matrix/" + c.ID() + "/app", SHA256: "deadbeef"})
		if err := appendJournal(dir, r.ID, *findCombination(r, c.ID())); err != nil {
			t.Fatal(err)
		}
	}

	// The snapshot on disk is still the pristine one, written before any
	// mutation. Replay must reconstruct the mutated state from the log alone.
	loaded, err := LoadRun(r.ID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	for _, c := range combos {
		got := findCombination(loaded, c.ID())
		if got == nil {
			t.Fatalf("combination %s missing after replay", c.ID())
		}
		if got.Status != "success" {
			t.Fatalf("combination %s status = %q, want success", c.ID(), got.Status)
		}
		if got.SHA256 != "deadbeef" {
			t.Fatalf("combination %s sha256 = %q", c.ID(), got.SHA256)
		}
	}
	if loaded.Succeeded != len(combos) {
		t.Fatalf("counters not replayed: succeeded = %d, want %d", loaded.Succeeded, len(combos))
	}
}

// A later entry for the same combination must win, so a running mark followed
// by a result is not reported as still running.
func TestJournal_LaterEntryWins(t *testing.T) {
	r := newTestRun(t, 2)
	dir := runDirOf(t, r)
	c := r.IncompleteCombinations()[0]

	r.MarkRunning(c.ID(), 1, time.Now())
	appendJournal(dir, r.ID, *findCombination(r, c.ID()))

	loaded, err := LoadRun(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := findCombination(loaded, c.ID()).Status; got != "running" {
		t.Fatalf("status = %q, want running", got)
	}

	r.RecordResult(Result{Combination: c, Status: "failed",
		Error: phelixerr.New(phelixerr.CodeBuildFailed, "boom")})
	appendJournal(dir, r.ID, *findCombination(r, c.ID()))

	loaded, err = LoadRun(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := findCombination(loaded, c.ID())
	if got.Status != "failed" || !strings.Contains(got.Error, "boom") {
		t.Fatalf("later entry did not win: %+v", got)
	}
}

// A line cut in half by a crash must be skipped, and the complete entries
// before it must still apply.
func TestJournal_TornTrailingLine(t *testing.T) {
	r := newTestRun(t, 3)
	dir := runDirOf(t, r)
	c := r.IncompleteCombinations()[0]

	r.RecordResult(Result{Combination: c, Status: "success", SHA256: "aaa"})
	err := appendJournal(dir, r.ID, *findCombination(r, c.ID()))
	if err != nil {
		return
	}

	// Simulate the crash: half a line, no newline.
	f, err := os.OpenFile(journalPathFor(dir, r.ID), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"c":"go1.1-li`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	loaded, err := LoadRun(r.ID)
	if err != nil {
		t.Fatalf("a torn line must not fail the load: %v", err)
	}
	if got := findCombination(loaded, c.ID()).Status; got != "success" {
		t.Fatalf("the complete entry before the torn line was lost: %q", got)
	}
}

// A wholly unreadable journal must be ignored, leaving the snapshot intact.
func TestJournal_GarbageIsIgnored(t *testing.T) {
	r := newTestRun(t, 3)
	dir := runDirOf(t, r)
	if err := os.WriteFile(journalPathFor(dir, r.ID), []byte("not json\nnor this\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadRun(r.ID)
	if err != nil {
		t.Fatalf("a garbage journal must not fail the load: %v", err)
	}
	if len(loaded.Combinations) != 3 {
		t.Fatalf("combinations lost: %d", len(loaded.Combinations))
	}
}

// An entry for a combination the run does not have must be ignored.
func TestJournal_UnknownCombinationIgnored(t *testing.T) {
	r := newTestRun(t, 2)
	dir := runDirOf(t, r)
	if err := appendJournal(dir, r.ID, RunCombination{ID: "does-not-exist", Status: "success"}); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadRun(r.ID); err != nil {
		t.Fatalf("an unknown combination must be ignored: %v", err)
	}
}

// The lister must not mistake the journal for a run record.
func TestJournal_NotMistakenForRunRecord(t *testing.T) {
	r := newTestRun(t, 2)
	dir := runDirOf(t, r)
	c := r.IncompleteCombinations()[0]
	err := appendJournal(dir, r.ID, *findCombination(r, c.ID()))
	if err != nil {
		return
	}

	runs, skipped, err := ListRuns()
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Fatalf("journal counted as a malformed record: skipped = %d", skipped)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
}

// Without a journal, loading must behave exactly as it did before.
func TestJournal_AbsentJournalIsNoOp(t *testing.T) {
	r := newTestRun(t, 4)
	loaded, err := LoadRun(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Combinations) != 4 || loaded.Total != 4 {
		t.Fatalf("absent-journal load changed behaviour: %+v", loaded)
	}
	if _, err := os.Stat(journalPathFor(runDirOf(t, r), r.ID)); !os.IsNotExist(err) {
		t.Fatal("a plain save must not create a journal")
	}
}
