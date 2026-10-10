package ops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
)

// TestLedger_CrossProcessBeginRaceExecutesOnce models two near-simultaneous
// `phelix` invocations (fresh Ledger instances sharing one on-disk ledger and
// lock file) applying the same request key. The blocking cross-process file
// lock plus the reload-under-lock must guarantee EXACTLY ONE execution; every
// other caller is rejected while the first is in flight or replays once it has
// completed — it never executes a second time. (Task 1.)
func TestLedger_CrossProcessBeginRaceExecutesOnce(t *testing.T) {
	setDataDir(t)
	const procs = 8
	fp := Fingerprint(KindRebuild, "demo", map[string]string{"strategy": "classic"})

	ledgers := make([]*Ledger, procs)
	for i := range ledgers {
		ledgers[i] = &Ledger{max: ledgerCapacity, entries: map[string]*KeyEntry{}}
	}

	var (
		mu       sync.Mutex
		executed int
		replayed int
		rejected int
		wg       sync.WaitGroup
	)
	start := make(chan struct{})
	for _, l := range ledgers {
		wg.Add(1)
		go func(l *Ledger) {
			defer wg.Done()
			<-start
			outcome, _, err := l.begin("race-key", fp, KindRebuild, "demo")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				rejected++
			case outcome == BeginExecute:
				executed++
				env, _ := machine.MarshalEnvelope(machine.Success("op_race0000000000", map[string]any{"ok": true}))
				if cerr := l.complete("race-key", "op_race0000000000", env); cerr != nil {
					t.Errorf("winner complete: %v", cerr)
				}
			case outcome == BeginReplay:
				replayed++
			}
		}(l)
	}
	close(start)
	wg.Wait()

	if executed != 1 {
		t.Fatalf("exactly one process must execute the key, got %d (replayed=%d rejected=%d)", executed, replayed, rejected)
	}
	if executed+replayed+rejected != procs {
		t.Fatalf("every outcome accounted for: %d+%d+%d != %d", executed, replayed, rejected, procs)
	}

	// A later begin, after the winner completed, replays the recorded result —
	// never a second execution.
	outcome, entry, err := ledgers[0].begin("race-key", fp, KindRebuild, "demo")
	if err != nil {
		t.Fatalf("post-completion begin: %v", err)
	}
	if outcome != BeginReplay {
		t.Fatalf("completed key must replay, got outcome %d", outcome)
	}
	var env machine.Envelope
	if err := json.Unmarshal(entry.Result, &env); err != nil {
		t.Fatalf("replayed result is not an envelope: %v", err)
	}
	if env.OperationID != "op_race0000000000" {
		t.Fatalf("replayed the wrong result: %+v", env)
	}
}

// TestLedger_TransientPersistErrorDoesNotPoison proves a transient IO failure
// fails only the current op; a later op re-initializes and succeeds. A
// structural corruption, by contrast, still poisons permanently. (Task 2.)
func TestLedger_TransientPersistErrorDoesNotPoison(t *testing.T) {
	setDataDir(t)
	l := &Ledger{max: ledgerCapacity, entries: map[string]*KeyEntry{}}
	fp := Fingerprint(KindRebuild, "demo", nil)

	fail := true
	persistHook = func() error {
		if fail {
			return fmt.Errorf("disk full (transient)")
		}
		return nil
	}
	t.Cleanup(func() { persistHook = nil })

	if _, _, err := l.begin("k1", fp, KindRebuild, "demo"); !phelixerr.IsCode(err, phelixerr.CodeUnavailable) {
		t.Fatalf("transient persist failure should surface as UNAVAILABLE, got %v", err)
	}

	// Not poisoned: once the disk recovers, the same instance re-initializes and
	// the first begin executes (its earlier attempt persisted nothing).
	fail = false
	outcome, _, err := l.begin("k1", fp, KindRebuild, "demo")
	if err != nil {
		t.Fatalf("after transient recovery, begin must succeed: %v", err)
	}
	if outcome != BeginExecute {
		t.Fatalf("recovered begin outcome = %d, want BeginExecute", outcome)
	}

	// Structural corruption DOES poison permanently.
	if err := os.WriteFile(l.path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.begin("k2", fp, KindRebuild, "demo"); !phelixerr.IsCode(err, phelixerr.CodeUnavailable) {
		t.Fatalf("corrupt ledger must fail closed, got %v", err)
	}
	if cerr := l.complete("k2", "op_x", []byte("{}")); !phelixerr.IsCode(cerr, phelixerr.CodeUnavailable) {
		t.Fatalf("poisoned ledger must keep failing closed, got %v", cerr)
	}
}

// TestLedger_FullOfInProgressReturnsUnavailable proves a ledger full of fresh
// in_progress entries refuses the next begin with UNAVAILABLE and never pushes
// the set past max in memory or on disk — the capacity-overflow bug. (Task 6.)
func TestLedger_FullOfInProgressReturnsUnavailable(t *testing.T) {
	setDataDir(t)
	l := &Ledger{max: 3, entries: map[string]*KeyEntry{}}
	for _, k := range []string{"a", "b", "c"} {
		o, _, err := l.begin(k, Fingerprint(KindRebuild, "demo", map[string]string{"k": k}), KindRebuild, "demo")
		if err != nil || o != BeginExecute {
			t.Fatalf("begin %s: outcome=%d err=%v", k, o, err)
		}
	}
	_, _, err := l.begin("d", Fingerprint(KindRebuild, "demo", map[string]string{"k": "d"}), KindRebuild, "demo")
	if !phelixerr.IsCode(err, phelixerr.CodeUnavailable) {
		t.Fatalf("full-of-in-progress begin = %v, want UNAVAILABLE", err)
	}
	l.mu.Lock()
	n := len(l.entries)
	l.mu.Unlock()
	if n != 3 {
		t.Fatalf("in-memory entries = %d, want 3 (never exceed max)", n)
	}
	raw, err := os.ReadFile(l.path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var disk ledgerFileData
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(disk.Entries) > 3 {
		t.Fatalf("persisted %d entries, want <= 3", len(disk.Entries))
	}
}

// TestLedger_OverCapacityFileLoadsByTrimming proves an over-capacity ledger
// file loads by keeping the newest max entries (never permanently poisoning),
// while a genuinely corrupt file still fails closed. (Task 6.)
func TestLedger_OverCapacityFileLoadsByTrimming(t *testing.T) {
	dir := setDataDir(t)
	opsDir := filepath.Join(dir, "ops")
	if err := os.MkdirAll(opsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const max = 3
	var entries []*KeyEntry
	for i := 0; i < 6; i++ {
		env, _ := machine.MarshalEnvelope(machine.Success(fmt.Sprintf("op_%016x", i), nil))
		entries = append(entries, &KeyEntry{
			Key:         fmt.Sprintf("over-%d", i),
			Fingerprint: "fp",
			State:       entryStateComplete,
			Result:      env,
			CreatedAt:   int64(i),
			UpdatedAt:   int64(i),
		})
	}
	blob, _ := json.MarshalIndent(ledgerFileData{Version: ledgerVersion, Entries: entries}, "", "  ")
	if err := os.WriteFile(filepath.Join(opsDir, ledgerFile), blob, 0o600); err != nil {
		t.Fatal(err)
	}

	l := &Ledger{max: max, entries: map[string]*KeyEntry{}}
	o, _, err := l.begin("over-5", "fp", KindRebuild, "demo")
	if err != nil {
		t.Fatalf("begin over newest key after trim: %v", err)
	}
	if o != BeginReplay {
		t.Fatalf("newest key must survive the trim and replay, got %d", o)
	}
	l.mu.Lock()
	n := len(l.entries)
	l.mu.Unlock()
	if n > max {
		t.Fatalf("trimmed in-memory set = %d, want <= %d", n, max)
	}

	// A genuinely corrupt file still fails closed.
	if err := os.WriteFile(filepath.Join(opsDir, ledgerFile), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	l2 := &Ledger{max: max, entries: map[string]*KeyEntry{}}
	if _, _, err := l2.begin("x", "fp", KindRebuild, "demo"); !phelixerr.IsCode(err, phelixerr.CodeUnavailable) {
		t.Fatalf("corrupt file must still fail closed, got %v", err)
	}
}
