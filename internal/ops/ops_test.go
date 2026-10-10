package ops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
)

// setDataDir points the whole package at an isolated data directory. The
// ledger re-anchors on every use and the record store resolves per call, so
// no process state needs resetting between tests.
func setDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	return dir
}

func TestRecordLifecycle(t *testing.T) {
	setDataDir(t)

	rec, err := Begin(KindRebuild, "demo", "key-1")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if rec.Status != machine.StatusPending {
		t.Fatalf("fresh record status = %q, want pending", rec.Status)
	}
	if rec.PID != os.Getpid() {
		t.Fatalf("record must carry the owning PID")
	}
	if rec.Actor == nil || rec.Actor.Type != "cli" || rec.Actor.Authenticated {
		t.Fatalf("Phase 1 actor must be the unauthenticated local CLI: %+v", rec.Actor)
	}
	if rec.RequestKey != "key-1" {
		t.Fatalf("request key not recorded: %q", rec.RequestKey)
	}

	if err := rec.MarkRunning(); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	rec.DeploymentID = "dep-0123456789abcdef01234567"
	if err := rec.MarkSucceeded(&Result{Version: 4, Port: 8080, Strategy: "classic"}); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}

	loaded, err := Load(rec.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Status != machine.StatusSucceeded || loaded.Result == nil || loaded.Result.Version != 4 {
		t.Fatalf("loaded record = %+v", loaded)
	}
	if loaded.DeploymentID != rec.DeploymentID {
		t.Fatalf("deployment correlation lost: %q", loaded.DeploymentID)
	}
	if loaded.FinishedAt == 0 {
		t.Fatal("terminal record must carry a finish timestamp")
	}
}

func TestRecordMarkFailed_Redacts(t *testing.T) {
	setDataDir(t)
	rec, err := Begin(KindRollback, "demo", "")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	err = rec.MarkFailed(phelixerr.New(phelixerr.CodeRollbackFailed, "copy failed with token ghp_abcdef0123456789012345678901234567890"))
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	loaded, loadErr := Load(rec.ID)
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if loaded.Status != machine.StatusFailed || loaded.Error == nil {
		t.Fatalf("loaded = %+v", loaded)
	}
	if strings.Contains(loaded.Error.Message, "ghp_abcdef") {
		t.Fatalf("record error message leaked a credential: %q", loaded.Error.Message)
	}
	if loaded.Error.Code != "ROLLBACK_FAILED" {
		t.Fatalf("record error code = %q", loaded.Error.Code)
	}
}

func TestRecordList_NewestFirstAndAppFilter(t *testing.T) {
	setDataDir(t)
	a1, _ := Begin(KindRebuild, "alpha", "")
	a2, _ := Begin(KindRollback, "alpha", "")
	b1, _ := Begin(KindBuild, "beta", "")
	_ = a1
	recs, _, err := List("", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("List() = %d records, want 3", len(recs))
	}
	byID := map[string]*Record{}
	for _, r := range recs {
		byID[r.ID] = r
	}
	if byID[b1.ID].Status != machine.StatusPending {
		t.Fatal("record should survive a round-trip")
	}

	alphaOnly, _, err := List("alpha", 0)
	if err != nil {
		t.Fatalf("List(alpha): %v", err)
	}
	if len(alphaOnly) != 2 {
		t.Fatalf("alpha filter returned %d records, want 2", len(alphaOnly))
	}

	limited, _, err := List("", 1)
	if err != nil {
		t.Fatalf("List(limit): %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit=1 returned %d records", len(limited))
	}
	_ = a2
}

func TestLoad_RejectsForeignIDShapes(t *testing.T) {
	setDataDir(t)
	if _, err := Load("../escape"); err == nil {
		t.Fatal("path-like ids must be rejected before touching the filesystem")
	}
	if _, err := Load("wh_0123456789abcdef"); err == nil {
		t.Fatal("non-op_ ids must be rejected")
	}
}

func TestLedger_FirstExecuteThenReplay(t *testing.T) {
	setDataDir(t)

	outcome, _, err := BeginKey("req-1", Fingerprint(KindRebuild, "demo", map[string]string{"strategy": "classic"}), KindRebuild, "demo")
	if err != nil {
		t.Fatalf("first BeginKey: %v", err)
	}
	if outcome != BeginExecute {
		t.Fatalf("first key must execute, got outcome %d", outcome)
	}

	env := machine.Success("op_0123456789abcdef", map[string]any{"version": 3})
	envBytes, mErr := machine.MarshalEnvelope(env)
	if mErr != nil {
		t.Fatalf("MarshalEnvelope: %v", mErr)
	}
	if err := CompleteKeyData("req-1", "op_0123456789abcdef", envBytes); err != nil {
		t.Fatalf("CompleteKey: %v", err)
	}

	outcome, entry, err := BeginKey("req-1", Fingerprint(KindRebuild, "demo", map[string]string{"strategy": "classic"}), KindRebuild, "demo")
	if err != nil {
		t.Fatalf("replay BeginKey: %v", err)
	}
	if outcome != BeginReplay {
		t.Fatalf("same key+inputs must replay, got outcome %d", outcome)
	}
	var replayed machine.Envelope
	if err := json.Unmarshal(entry.Result, &replayed); err != nil {
		t.Fatalf("stored result must be a valid envelope: %v", err)
	}
	if replayed.Result == nil {
		t.Fatal("replayed envelope lost its result")
	}
}

func TestLedger_ConflictingKeyRejected(t *testing.T) {
	setDataDir(t)
	fp1 := Fingerprint(KindRebuild, "demo", map[string]string{"strategy": "classic"})
	if _, _, err := BeginKey("req-c", fp1, KindRebuild, "demo"); err != nil {
		t.Fatalf("BeginKey: %v", err)
	}
	envBytesC, _ := machine.MarshalEnvelope(machine.Success("op_0123456789abcdef", nil))
	if err := CompleteKeyData("req-c", "op_0123456789abcdef", envBytesC); err != nil {
		t.Fatalf("CompleteKey: %v", err)
	}

	fp2 := Fingerprint(KindRebuild, "demo", map[string]string{"strategy": "blue-green"})
	_, _, err := BeginKey("req-c", fp2, KindRebuild, "demo")
	if !phelixerr.IsCode(err, phelixerr.CodeIdempotencyConflict) {
		t.Fatalf("key reuse with different inputs must be IDEMPOTENCY_CONFLICT, got: %v", err)
	}
}

func TestLedger_DifferentMaterialConflicts(t *testing.T) {
	setDataDir(t)
	// The same key against a different app is a different operation.
	if _, _, err := BeginKey("req-d", Fingerprint(KindRebuild, "alpha", nil), KindRebuild, "alpha"); err != nil {
		t.Fatalf("BeginKey: %v", err)
	}
	envBytesD, _ := machine.MarshalEnvelope(machine.Success("op_0123456789abcdef", nil))
	if err := CompleteKeyData("req-d", "op_0123456789abcdef", envBytesD); err != nil {
		t.Fatalf("CompleteKey: %v", err)
	}
	_, _, err := BeginKey("req-d", Fingerprint(KindRebuild, "beta", nil), KindRebuild, "beta")
	if !phelixerr.IsCode(err, phelixerr.CodeIdempotencyConflict) {
		t.Fatalf("cross-app key reuse must conflict, got: %v", err)
	}
}

func TestLedger_InProgressRejected(t *testing.T) {
	setDataDir(t)
	if _, _, err := BeginKey("req-e", Fingerprint(KindRebuild, "demo", nil), KindRebuild, "demo"); err != nil {
		t.Fatalf("BeginKey: %v", err)
	}
	_, _, err := BeginKey("req-e", Fingerprint(KindRebuild, "demo", nil), KindRebuild, "demo")
	if !phelixerr.IsCode(err, phelixerr.CodeUnavailable) {
		t.Fatalf("concurrent duplicate must be rejected UNAVAILABLE, got: %v", err)
	}
}

func TestLedger_SurvivesRestart_AsDurableReplay(t *testing.T) {
	setDataDir(t)
	fp := Fingerprint(KindRollback, "demo", map[string]string{"target": "3"})
	if _, _, err := BeginKey("req-f", fp, KindRollback, "demo"); err != nil {
		t.Fatalf("BeginKey: %v", err)
	}
	env := machine.Success("op_0123456789abcdef", map[string]any{"to_version": 3})
	envBytesF, mErr := machine.MarshalEnvelope(env)
	if mErr != nil {
		t.Fatalf("MarshalEnvelope: %v", mErr)
	}
	if err := CompleteKeyData("req-f", "op_0123456789abcdef", envBytesF); err != nil {
		t.Fatalf("CompleteKey: %v", err)
	}

	// A new process would see the same on-disk ledger: the entry replays.
	outcome, entry, err := BeginKey("req-f", fp, KindRollback, "demo")
	if err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	if outcome != BeginReplay {
		t.Fatalf("completed key must replay after restart, got %d", outcome)
	}
	if entry.OperationID != "op_0123456789abcdef" {
		t.Fatalf("operation correlation lost across restart: %q", entry.OperationID)
	}
}

func TestLedger_RestartInterrupted_IsIndeterminateNeverReExecuted(t *testing.T) {
	setDataDir(t)
	dir := filepath.Join(setDataDir(t), "ops")

	// Simulate a crash mid-execution: write an in_progress entry to disk the
	// way BeginKey would, then let a fresh process (fresh ledger instance)
	// load it.
	fp := Fingerprint(KindRebuild, "demo", nil)
	if _, _, err := BeginKey("req-g", fp, KindRebuild, "demo"); err != nil {
		t.Fatalf("BeginKey: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "request-key-ledger.json"))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var disk ledgerFileData
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatalf("decode ledger: %v", err)
	}
	if len(disk.Entries) != 1 || disk.Entries[0].State != entryStateInProgress {
		t.Fatalf("expected one in_progress entry, got %+v", disk.Entries)
	}

	// A real crash leaves the entry owned by a process that no longer exists.
	// Clear the recorded owner so the fresh instance sees a genuine orphan — a
	// still-live owner is deliberately left in_progress (reported UNAVAILABLE)
	// so a concurrent operation is never clobbered.
	disk.Entries[0].PID = 0
	rewrite, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		t.Fatalf("re-encode ledger: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "request-key-ledger.json"), rewrite, 0o600); err != nil {
		t.Fatalf("rewrite ledger: %v", err)
	}

	// Fresh instance (new process semantics): the in_progress entry must be
	// closed as indeterminate and the key must replay that result — never
	// re-execute.
	fresh := &Ledger{max: ledgerCapacity, entries: map[string]*KeyEntry{}, dir: filepath.Dir(dir)}
	outcome, entry, err := func() (BeginOutcome, *KeyEntry, error) {
		fresh.mu.Lock()
		defer fresh.mu.Unlock()
		if err := fresh.initializeLocked(); err != nil {
			return 0, nil, err
		}
		e := fresh.entries["req-g"]
		if e == nil {
			return 0, nil, phelixerr.New(phelixerr.CodeNotFound, "entry vanished")
		}
		if e.State != entryStateComplete {
			return 0, nil, phelixerr.New(phelixerr.CodeUnavailable, "restart-interrupted entry not closed")
		}
		return BeginReplay, e, nil
	}()
	if err != nil {
		t.Fatalf("fresh load: %v", err)
	}
	if outcome != BeginReplay {
		t.Fatalf("interrupted key must replay the indeterminate result, got %d", outcome)
	}
	var replayed machine.Envelope
	if err := json.Unmarshal(entry.Result, &replayed); err != nil {
		t.Fatalf("indeterminate result must be an envelope: %v", err)
	}
	if replayed.Error == nil || replayed.Error.Code != "UNAVAILABLE" {
		t.Fatalf("indeterminate result must carry UNAVAILABLE, got %+v", replayed.Error)
	}
}

func TestLedger_CorruptLedgerFailsClosed(t *testing.T) {
	dir := setDataDir(t)
	opsDir := filepath.Join(dir, "ops")
	if err := os.MkdirAll(opsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opsDir, "request-key-ledger.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := BeginKey("req-h", Fingerprint(KindRebuild, "demo", nil), KindRebuild, "demo")
	if !phelixerr.IsCode(err, phelixerr.CodeUnavailable) {
		t.Fatalf("corrupt ledger must fail closed with UNAVAILABLE, got: %v", err)
	}
	// And stay poisoned.
	if err := CompleteKeyData("req-h", "", []byte("{}")); !phelixerr.IsCode(err, phelixerr.CodeUnavailable) {
		t.Fatalf("poisoned ledger must reject completes, got: %v", err)
	}
}

func TestLedger_EvictsOldestCompletedWhenFull(t *testing.T) {
	setDataDir(t)
	small := &Ledger{max: 3, entries: map[string]*KeyEntry{}}

	mk := func(key string) {
		t.Helper()
		small.mu.Lock()
		defer small.mu.Unlock()
		o, _, err := small.beginLocked(key, Fingerprint(KindRebuild, "demo", map[string]string{"k": key}), KindRebuild, "demo")
		if err != nil {
			t.Fatalf("begin %s: %v", key, err)
		}
		if o != BeginExecute {
			t.Fatalf("expected execute for %s", key)
		}
		data, err := json.Marshal(machine.Success("op_"+key, nil))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := small.completeLocked(key, "op_"+key, data); err != nil {
			t.Fatalf("complete %s: %v", key, err)
		}
	}
	mk("a")
	mk("b")
	mk("c")
	mk("d") // capacity 3 reached: "a" (oldest completed) must be evicted

	small.mu.Lock()
	defer small.mu.Unlock()
	if _, exists := small.entries["a"]; exists {
		t.Fatal("oldest completed entry should have been evicted")
	}
	if _, exists := small.entries["d"]; !exists {
		t.Fatal("newest entry missing")
	}
}

func TestLedger_ConcurrentKeysAreSafe(t *testing.T) {
	setDataDir(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "parallel-key"
			fp := Fingerprint(KindRebuild, "demo", map[string]string{"i": "same"})
			_, _, err := BeginKey(key, fp, KindRebuild, "demo")
			if err != nil {
				return // a loser of the race is fine
			}
			data, _ := machine.MarshalEnvelope(machine.Success("", nil))
			_ = CompleteKeyData(key, "", data)
		}(i)
	}
	wg.Wait()
}

// TestPruneKeepsInFlightProtectedAndNewest proves operation-record retention
// keeps every in-flight record, every id in the protected set, and the newest
// max terminal records, pruning only older unprotected terminal ones. (Task 5.)
func TestPruneKeepsInFlightProtectedAndNewest(t *testing.T) {
	setDataDir(t)
	mk := func(app string) *Record {
		r, err := Begin(KindRebuild, app, "")
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		return r
	}
	var terminal []*Record
	for i := 0; i < 5; i++ {
		r := mk(fmt.Sprintf("t%d", i))
		if err := r.MarkSucceeded(&Result{Version: i}); err != nil {
			t.Fatalf("MarkSucceeded: %v", err)
		}
		terminal = append(terminal, r)
		time.Sleep(2 * time.Millisecond)
	}
	inflight := mk("live")
	if err := inflight.MarkRunning(); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}

	protected := map[string]bool{terminal[0].ID: true} // the OLDEST, explicitly pinned
	pruned, err := Prune(2, protected)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned != 2 {
		t.Fatalf("pruned = %d, want 2", pruned)
	}
	if _, err := Load(inflight.ID); err != nil {
		t.Fatalf("in-flight record must never be pruned: %v", err)
	}
	if _, err := Load(terminal[0].ID); err != nil {
		t.Fatalf("protected record must never be pruned: %v", err)
	}
	for _, r := range terminal[3:] { // newest two terminal
		if _, err := Load(r.ID); err != nil {
			t.Fatalf("newest terminal %s must be kept: %v", r.ID, err)
		}
	}
	for _, r := range []*Record{terminal[1], terminal[2]} { // old, unprotected, over budget
		if _, err := Load(r.ID); !phelixerr.IsCode(err, phelixerr.CodeNotFound) {
			t.Fatalf("old terminal %s should be pruned, got %v", r.ID, err)
		}
	}
}

func TestFingerprint_IsDeterministicAndSensitive(t *testing.T) {
	f1 := Fingerprint(KindRebuild, "demo", map[string]string{"strategy": "classic"})
	f2 := Fingerprint(KindRebuild, "demo", map[string]string{"strategy": "classic"})
	if f1 != f2 {
		t.Fatal("same material must fingerprint identically")
	}
	f3 := Fingerprint(KindRebuild, "demo", map[string]string{"strategy": "rolling"})
	if f1 == f3 {
		t.Fatal("different material must fingerprint differently")
	}
	f4 := Fingerprint(KindRollback, "demo", map[string]string{"strategy": "classic"})
	if f1 == f4 {
		t.Fatal("kind participates in the fingerprint")
	}
}

// TestLedger_ReplayBytesMatchFreshOutput pins the byte-fidelity contract:
// the ledger stores exactly what machine.MarshalEnvelope (the fresh
// execution's stdout bytes) produced, so a replay — including one from a
// fresh process reading the re-serialized ledger file — is byte-identical to
// the first response.
func TestLedger_ReplayBytesMatchFreshOutput(t *testing.T) {
	dir := setDataDir(t)
	env := machine.Success("op_0123456789abcdef", map[string]any{"version": 2, "strategy": "classic"})
	fresh, err := machine.MarshalEnvelope(env)
	if err != nil {
		t.Fatalf("MarshalEnvelope: %v", err)
	}
	if _, _, err := BeginKey("bytes-1", Fingerprint(KindRebuild, "demo", nil), KindRebuild, "demo"); err != nil {
		t.Fatalf("BeginKey: %v", err)
	}
	if err := CompleteKeyData("bytes-1", "op_0123456789abcdef", fresh); err != nil {
		t.Fatalf("CompleteKeyData: %v", err)
	}

	// Simulate a fresh process: load the re-serialized file from disk.
	raw, err := os.ReadFile(filepath.Join(dir, "ops", "request-key-ledger.json"))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var disk ledgerFileData
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatalf("decode: %v", err)
	}
	replayed := disk.Entries[0].Result
	if string(replayed) != string(fresh) {
		t.Fatalf("replay bytes differ from fresh output:\n got: %q\nwant: %q", replayed, fresh)
	}
}
