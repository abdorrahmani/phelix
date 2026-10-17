package matrix

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// journalSuffix names the append-only delta log written beside a run record.
// It is deliberately not ".json" so the run lister, which filters on that
// extension, cannot mistake it for a record.
const journalSuffix = ".jsonl"

// journalMaxLine bounds one journal line. A combination entry is far under
// this; the limit exists so a corrupt file cannot make replay allocate
// without bound.
const journalMaxLine = 4 * 1024 * 1024

// journalEntry is one recorded mutation: the combination entry as it stood
// after the mutation. Replaying entries in order reproduces the run's
// combination state exactly, because a later entry for the same ID replaces
// an earlier one.
type journalEntry struct {
	Combination string         `json:"c"`
	Entry       RunCombination `json:"rc"`
}

// journalPathFor is the delta log for one run.
func journalPathFor(dir string, id RunID) string {
	return filepath.Join(dir, string(id)+journalSuffix)
}

// appendJournal records one combination's post-mutation state. To append is
// intentionally not atomic: a torn trailing line is discarded on replay, and
// the next snapshot supersedes the log, so paying for atomicity would buy
// nothing.
func appendJournal(dir string, id RunID, rc RunCombination) error {
	line, err := json.Marshal(journalEntry{Combination: rc.ID, Entry: rc})
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "encode matrix journal entry", err)
	}
	line = append(line, '\n')

	f, err := os.OpenFile(journalPathFor(dir, id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "open matrix run journal %s", id)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Write(line); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "append matrix run journal %s", id)
	}
	return nil
}

// replayJournal applies every readable entry to run, in order.
//
// A malformed line is skipped rather than failing the load: the only thing
// that produces one is a crash mid-append, and an interrupted write must
// never make a run unreadable. Skipping is safe because the remaining lines
// are still valid state and the snapshot bounds how wrong the result can be.
func replayJournal(dir string, id RunID, run *Run) {
	f, err := os.Open(journalPathFor(dir, id))
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	index := make(map[string]int, len(run.Combinations))
	for i := range run.Combinations {
		index[run.Combinations[i].ID] = i
	}

	applied := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), journalMaxLine)
	for sc.Scan() {
		var e journalEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		i, ok := index[e.Combination]
		if !ok {
			continue
		}
		run.Combinations[i] = e.Entry
		applied = true
	}

	if !applied {
		return
	}

	run.mu.Lock()
	run.recountLocked()
	run.mu.Unlock()
}

// discardJournal removes the delta log once its state is in the snapshot.
// Best-effort: a leftover log is harmless because replay is idempotent.
func discardJournal(dir string, id RunID) {
	_ = os.Remove(journalPathFor(dir, id))
}
