package ops

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/server"
)

const (
	// ledgerVersion is the on-disk schema of the request-key ledger. v2
	// stores replay results base64-encoded (v1's embedded raw JSON was
	// re-indented by MarshalIndent, breaking byte-for-byte replays). The
	// feature shipped within the same release cycle, so no v1 files are
	// migrated: they fail closed with a version error.
	ledgerVersion = 2
	// ledgerCapacity bounds the ledger. Unlike the remote-command ledgers —
	// whose unevicted 256-entry ceiling the Phase 0 audit flagged as a
	// liveness risk — the request-key ledger evicts its oldest COMPLETED
	// entries when full. In-progress entries are never evicted: evicting one
	// would allow the same key to execute twice.
	ledgerCapacity = 512
	// ledgerFile lives under <DataDir>/ops/ so all operation state sits in
	// one place.
	ledgerFile = "request-key-ledger.json"
	// ledgerLockFile is the cross-process mutex for the begin/complete
	// critical sections. It is never renamed, so a blocking flock held on it
	// protects the ledger across its atomic replacement — the same pattern the
	// session store uses (internal/session/lock_unix.go).
	ledgerLockFile = "request-key-ledger.lock"

	entryStateInProgress = "in_progress"
	entryStateComplete   = "complete"

	// abandonedInProgressAge is how old an in_progress entry must be before a
	// full ledger may reclaim its slot. A deploy/rollback (even with a
	// verification window) finishes well inside this bound, so only genuinely
	// stuck entries from a crashed process are reclaimed — never a live op.
	abandonedInProgressAge = time.Hour
)

// b64Bytes serializes as a base64 string. The ledger file is written with
// MarshalIndent, which re-indents embedded JSON values — a stored envelope
// kept as a raw JSON value would be reformatted on disk and a fresh process
// would replay mutated bytes. As a base64 string the stored envelope is
// opaque to the encoder and replays byte-for-byte, across restarts included.
type b64Bytes []byte

func (b b64Bytes) MarshalJSON() ([]byte, error) {
	return json.Marshal(base64.StdEncoding.EncodeToString(b))
}

func (b *b64Bytes) UnmarshalJSON(data []byte) error {
	var enc string
	if err := json.Unmarshal(data, &enc); err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return err
	}
	*b = raw
	return nil
}

// KeyEntry is one durably-recorded request key. Result holds the exact
// envelope bytes the terminal execution produced, so a replay is
// byte-for-byte identical to the first response.
type KeyEntry struct {
	Key         string   `json:"key"`
	Fingerprint string   `json:"fingerprint"`
	Kind        string   `json:"kind,omitempty"`
	App         string   `json:"app,omitempty"`
	OperationID string   `json:"operation_id,omitempty"`
	State       string   `json:"state"`
	Result      b64Bytes `json:"result_b64,omitempty"`
	// PID is the process that owns an in_progress entry. It distinguishes a
	// crashed owner (dead PID → the entry is orphaned → closed as indeterminate
	// on load) from a concurrently-live owner (alive PID → left in_progress so a
	// duplicate begin sees UNAVAILABLE, never clobbering a running operation).
	// A pre-PID ledger entry (PID 0) is treated as orphaned, which is the right
	// behavior for an entry left by an older build. Metadata only: it is never
	// part of the fingerprint or the replayed Result bytes.
	PID       int   `json:"pid,omitempty"`
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

type ledgerFileData struct {
	Version int         `json:"version"`
	Entries []*KeyEntry `json:"entries"`
}

// BeginOutcome tells the caller what to do after BeginKey.
type BeginOutcome int

const (
	// BeginExecute: no prior use of this key — execute the mutation, then
	// call CompleteKey with the terminal envelope.
	BeginExecute BeginOutcome = iota
	// BeginReplay: this exact request already completed — do not execute;
	// deliver entry.Result verbatim.
	BeginReplay
)

// Ledger is the durable request-key idempotency ledger. It generalizes the
// remote-command ledger (internal/grpc/rollback_command.go) with the same
// begin/complete lifecycle, the same fail-closed corruption handling, and the
// same restart semantics: an entry still in_progress after a restart is
// closed as indeterminate — the mutation is never re-executed under a key
// whose outcome is unknown.
type Ledger struct {
	mu sync.Mutex
	// dir is the data directory the ledger was last loaded from. The path is
	// re-resolved from the current data directory on every use (mirroring
	// InitializeRollbackLedger) so a test or one-shot CLI whose
	// PHELIX_DATA_DIR/HOME changed since process start re-anchors the ledger
	// instead of trusting a stale eager path.
	dir      string
	path     string
	lockPath string
	max      int
	entries  map[string]*KeyEntry
	order    []string
	// loaded records whether this process has performed its first load. The
	// first load closes entries left in_progress by a crashed PRIOR process as
	// indeterminate (restart semantics); later loads only refresh the in-memory
	// view from disk and never touch in_progress entries, so a concurrently
	// live process's entry is never wrongly closed.
	loaded bool
	// corrupt is a PERMANENT poison set only by structural corruption (bad
	// version, invalid/duplicate entry, completed-without-result). A transient
	// IO failure (read/write/sync) is NOT recorded here: it fails the current
	// op but a later call re-initializes from disk, so one disk hiccup can no
	// longer brick idempotency for a long-lived host's whole lifetime.
	corrupt error
}

// keyLedger is the CLI-wide ledger. It initializes lazily on first use (the
// CLI is a one-shot process; there is no daemon startup hook) and, once
// poisoned by a load or persist failure, stays fail-closed: every keyed
// operation answers UNAVAILABLE rather than running without idempotency.
var keyLedger = &Ledger{
	max:     ledgerCapacity,
	entries: make(map[string]*KeyEntry),
}

// KeyLedgerPath exposes the effective ledger location for diagnostics.
func KeyLedgerPath() string {
	keyLedger.mu.Lock()
	defer keyLedger.mu.Unlock()
	keyLedger.anchorLocked()
	return keyLedger.path
}

// Fingerprint derives the idempotency fingerprint for a mutation request.
// material carries the operation-defining inputs (kind and app are included
// by the caller); any change to a material input yields a different
// fingerprint, which turns a key reuse into IDEMPOTENCY_CONFLICT instead of a
// silent replay of the wrong operation.
func Fingerprint(kind, app string, material map[string]string) string {
	canonical := make(map[string]string, len(material)+2)
	for k, v := range material {
		canonical[k] = v
	}
	canonical["kind"] = kind
	canonical["app"] = app
	data, err := json.Marshal(canonical)
	if err != nil { // map[string]string cannot fail to marshal, but stay total
		data = []byte(fmt.Sprintf("%v", canonical))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// BeginKey consults the ledger for a request key. See BeginOutcome for the
// outcomes. Errors mean the mutation must not execute:
//   - IDEMPOTENCY_CONFLICT: the key already completed a materially different
//     operation (fingerprint mismatch).
//   - UNAVAILABLE: the same key is currently in progress (another process is
//     executing it), the ledger is full of in-progress operations, or the
//     ledger is corrupt (fail-closed).
//
// The whole begin sequence runs under a blocking cross-process file lock, and
// the ledger is re-read from disk under that lock, so two processes starting
// near-simultaneously can never both observe "no entry" and both execute: the
// late caller sees the first's in_progress entry (UNAVAILABLE) or, once the
// first has completed, its result (replay).
func BeginKey(key, fingerprint, kind, app string) (BeginOutcome, *KeyEntry, error) {
	return keyLedger.begin(key, fingerprint, kind, app)
}

// begin runs the begin sequence on an instance under its in-process mutex and
// the cross-process file lock. Exposed on the receiver (not only via the
// package-level BeginKey) so a test can model two independent "processes" as
// two Ledger instances sharing one on-disk ledger.
func (l *Ledger) begin(key, fingerprint, kind, app string) (BeginOutcome, *KeyEntry, error) {
	if key == "" {
		return BeginExecute, nil, phelixerr.New(phelixerr.CodeInvalidArgument, "empty request key")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.anchorLocked()
	unlock, err := l.acquireFileLockLocked()
	if err != nil {
		return 0, nil, err
	}
	defer unlock()
	return l.beginLocked(key, fingerprint, kind, app)
}

// beginLocked implements the begin semantics with the mutex already held.
// It (re)loads the ledger from disk first so the decision is made against the
// authoritative on-disk state, not a possibly-stale in-memory cache.
func (l *Ledger) beginLocked(key, fingerprint, kind, app string) (BeginOutcome, *KeyEntry, error) {
	if err := l.initializeLocked(); err != nil {
		return 0, nil, err
	}
	if existing := l.entries[key]; existing != nil {
		if existing.Fingerprint != fingerprint {
			return 0, nil, phelixerr.Newf(phelixerr.CodeIdempotencyConflict,
				"request key %q was already used for a %s operation on %q (%s); supply a new request key for a different operation",
				key, kindOr(existing.Kind), appOr(existing.App), operationRef(existing))
		}
		if existing.State == entryStateInProgress {
			return 0, nil, phelixerr.Newf(phelixerr.CodeUnavailable,
				"request key %q is already in progress (operation %s); query it instead of re-executing",
				key, operationRef(existing))
		}
		return BeginReplay, existing, nil
	}
	// Never exceed capacity in memory: when every slot is a fresh in_progress
	// entry, refuse rather than append past max (which would later persist an
	// over-capacity file and poison the next load). This is a transient
	// condition — the caller retries once an in-flight op completes.
	if !l.makeRoomLocked() {
		return 0, nil, phelixerr.New(phelixerr.CodeUnavailable,
			"request-key ledger is full of in-progress operations; retry shortly")
	}
	now := time.Now().UnixMilli()
	entry := &KeyEntry{
		Key:         key,
		Fingerprint: fingerprint,
		Kind:        kind,
		App:         app,
		State:       entryStateInProgress,
		PID:         os.Getpid(),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	l.entries[key] = entry
	l.order = append(l.order, key)
	if err := l.persistLocked(); err != nil {
		// Transient persist failure: roll back the in-memory append and return
		// the error WITHOUT poisoning. The next call re-initializes from disk
		// (which the failed atomic write never touched), so a single disk
		// hiccup cannot permanently disable idempotency.
		delete(l.entries, key)
		l.order = l.order[:len(l.order)-1]
		return 0, nil, err
	}
	return BeginExecute, entry, nil
}

// CompleteKeyData records the terminal envelope for a key. data is the exact
// byte string the fresh execution printed (machine.MarshalEnvelope output)
// and is replayed byte-for-byte on repeated requests, whether the operation
// succeeded or failed — a consumed key never re-executes. The whole complete
// sequence runs under the cross-process file lock and re-reads the ledger from
// disk, so it observes keys written by concurrent processes.
func CompleteKeyData(key, operationID string, data []byte) error {
	if key == "" || len(data) == 0 {
		return nil
	}
	return keyLedger.complete(key, operationID, data)
}

// complete runs the complete sequence on an instance under its in-process
// mutex and the cross-process file lock.
func (l *Ledger) complete(key, operationID string, data []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.anchorLocked()
	unlock, err := l.acquireFileLockLocked()
	if err != nil {
		return err
	}
	defer unlock()
	return l.completeLocked(key, operationID, data)
}

// completeLocked implements the complete semantics with the mutex already
// held and the envelope already marshaled.
func (l *Ledger) completeLocked(key, operationID string, data []byte) error {
	if err := l.initializeLocked(); err != nil {
		return err
	}
	e := l.entries[key]
	if e == nil || e.State != entryStateInProgress {
		return phelixerr.New(phelixerr.CodeUnavailable, "request-key ledger has no in-progress entry for this key")
	}
	e.OperationID = operationID
	e.State = entryStateComplete
	e.Result = data
	e.UpdatedAt = time.Now().UnixMilli()
	if err := l.persistLocked(); err != nil {
		// Transient persist failure: do NOT poison. The on-disk entry is still
		// in_progress (the atomic write never landed), so a later complete call
		// re-reads it and can record the terminal result again.
		return err
	}
	return nil
}

// makeRoomLocked ensures there is room for one more entry. It evicts the
// oldest COMPLETED entries first; only if none can be evicted does it reclaim
// one abandoned in_progress entry (older than abandonedInProgressAge, i.e. a
// crashed process's orphan) by closing it as indeterminate so the next
// iteration can evict it. It returns false when the ledger is full of fresh
// in_progress entries and no slot can be freed — evicting one of those would
// let its key execute twice, so the caller must be told to retry instead.
func (l *Ledger) makeRoomLocked() bool {
	for len(l.entries) >= l.max {
		if l.evictOneCompletedLocked() {
			continue
		}
		if l.closeOneAbandonedLocked() {
			continue
		}
		return false
	}
	return true
}

// evictOneCompletedLocked drops the oldest completed entry, if any, and
// reports whether it removed one. In-progress entries are never evicted — that
// would let a key execute twice.
func (l *Ledger) evictOneCompletedLocked() bool {
	for i, key := range l.order {
		e := l.entries[key]
		if e == nil || e.State != entryStateComplete {
			continue
		}
		delete(l.entries, key)
		l.order = append(l.order[:i], l.order[i+1:]...)
		return true
	}
	return false
}

// closeOneAbandonedLocked closes the oldest in_progress entry that is older
// than abandonedInProgressAge as indeterminate, so a transient burst of stuck
// entries from crashed processes can self-heal. A reclaimed entry becomes a
// completed one (its key replays the indeterminate result, never re-executes)
// and is evicted on the next makeRoomLocked iteration. Returns whether it
// closed one.
func (l *Ledger) closeOneAbandonedLocked() bool {
	cutoff := time.Now().Add(-abandonedInProgressAge).UnixMilli()
	for _, key := range l.order {
		e := l.entries[key]
		if e == nil || e.State != entryStateInProgress || e.UpdatedAt > cutoff {
			continue
		}
		e.State = entryStateComplete
		e.UpdatedAt = time.Now().UnixMilli()
		e.Result = indeterminateResult(e)
		return true
	}
	return false
}

// anchorLocked re-resolves the ledger paths from the current data directory.
// A directory switch (test isolation, PHELIX_DATA_DIR override) resets the
// in-memory view — including any poison — so the new location is a clean
// authoritative slate.
func (l *Ledger) anchorLocked() {
	if dir := server.DataDir(); dir != l.dir {
		l.dir = dir
		l.loaded = false
		l.entries = make(map[string]*KeyEntry)
		l.order = nil
		l.corrupt = nil
	}
	l.path = filepath.Join(l.dir, "ops", ledgerFile)
	l.lockPath = filepath.Join(l.dir, "ops", ledgerLockFile)
}

// acquireFileLockLocked takes the blocking cross-process ledger lock and
// returns a release func. A failure here is transient (an unwritable data
// directory) and never poisons the instance. The caller already holds the
// in-process mutex and has anchored the paths.
func (l *Ledger) acquireFileLockLocked() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(l.lockPath), 0o700); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeUnavailable, "create request-key ledger directory", err)
	}
	f, err := os.OpenFile(l.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeUnavailable, "open request-key ledger lock", err)
	}
	if err := lockFileBlocking(f); err != nil {
		_ = f.Close()
		return nil, phelixerr.Wrap(phelixerr.CodeUnavailable, "acquire request-key ledger lock", err)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}

// initializeLocked makes the in-memory view authoritative before a begin or
// complete decides anything. Mirroring the remote-command ledger, an
// undecodable, wrong-version or invalid ledger poisons the instance
// permanently (fail-closed). A transient IO error fails only the current call.
//
// It is called on every begin/complete (under the cross-process lock) and so
// re-reads the ledger from disk each time: this both closes the begin race and
// keeps a long-lived host (phelix mcp serve) from diverging from keys written
// by concurrent CLI runs. The indeterminate-close of restart-interrupted
// in_progress entries runs ONLY on the first load in this process, so a
// concurrently live process's in_progress entry is never wrongly closed on a
// refresh.
func (l *Ledger) initializeLocked() error {
	l.anchorLocked()
	if l.corrupt != nil {
		return l.corrupt
	}
	firstInit := !l.loaded
	if err := l.loadLocked(); err != nil {
		return err
	}
	if firstInit {
		if err := l.closeOrphanedInProgressLocked(); err != nil {
			return err
		}
		l.loaded = true
	}
	return nil
}

// closeOrphanedInProgressLocked closes entries left in_progress by a crashed
// PRIOR process as indeterminate — the outcome is unknown and the mutation is
// never re-executed under that key (fail-closed). It runs only on first load,
// and converts ONLY entries whose owning process is no longer alive: a
// concurrently-live process's in_progress entry is left untouched (a duplicate
// begin sees it and returns UNAVAILABLE), so a running operation is never
// clobbered by another process starting up.
func (l *Ledger) closeOrphanedInProgressLocked() error {
	changed := false
	now := time.Now().UnixMilli()
	for _, key := range l.order {
		e := l.entries[key]
		if e.State != entryStateInProgress || pidAlive(e.PID) {
			continue
		}
		e.State = entryStateComplete
		e.UpdatedAt = now
		e.Result = indeterminateResult(e)
		changed = true
	}
	if changed {
		if err := l.persistLocked(); err != nil {
			return err
		}
	}
	return nil
}

// pidAlive reports whether a process with pid exists. Signal 0 probes without
// delivering anything; EPERM means the process is alive but owned by another
// user. A non-positive pid (unknown/legacy owner) is treated as not alive, so
// an in_progress entry with no recorded owner is recovered as an orphan.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// poison records a permanent structural-corruption failure and returns it. A
// poisoned ledger answers UNAVAILABLE for every keyed operation (fail-closed)
// until the data directory changes (anchorLocked resets it).
func (l *Ledger) poison(err error) error {
	l.corrupt = err
	return err
}

// loadLocked re-reads the ledger file into a fresh in-memory view. It is
// called on every begin/complete, so it always resets entries/order first.
// Structural corruption poisons the instance permanently; a transient read
// error is returned without poisoning. An over-capacity file is trimmed to the
// newest entries (the decision-log discipline) rather than poisoned, so an
// older over-cap bug cannot wedge the ledger forever.
func (l *Ledger) loadLocked() error {
	l.entries = make(map[string]*KeyEntry)
	l.order = nil
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		if mkErr := os.MkdirAll(filepath.Dir(l.path), 0o700); mkErr != nil {
			return phelixerr.Wrap(phelixerr.CodeUnavailable, "create request-key ledger directory", mkErr)
		}
		return nil
	}
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "read request-key ledger", err)
	}
	var disk ledgerFileData
	if err := json.Unmarshal(data, &disk); err != nil {
		return l.poison(phelixerr.Wrap(phelixerr.CodeUnavailable, "decode request-key ledger", err))
	}
	if disk.Version != ledgerVersion {
		return l.poison(phelixerr.Newf(phelixerr.CodeUnavailable, "unsupported request-key ledger version %d", disk.Version))
	}
	entries := disk.Entries
	if len(entries) > l.max {
		// Over capacity: keep the newest max entries rather than failing closed
		// forever. order is append-order (oldest first), so the tail is newest.
		entries = entries[len(entries)-l.max:]
	}
	for _, e := range entries {
		if e == nil || e.Key == "" || e.Fingerprint == "" || (e.State != entryStateInProgress && e.State != entryStateComplete) {
			return l.poison(phelixerr.New(phelixerr.CodeUnavailable, "request-key ledger contains an invalid entry"))
		}
		if e.State == entryStateComplete && len(e.Result) == 0 {
			return l.poison(phelixerr.New(phelixerr.CodeUnavailable, "request-key ledger completed entry has no result"))
		}
		if _, exists := l.entries[e.Key]; exists {
			return l.poison(phelixerr.New(phelixerr.CodeUnavailable, "request-key ledger contains duplicate key"))
		}
		l.entries[e.Key] = e
		l.order = append(l.order, e.Key)
	}
	return nil
}

// persistHook, when non-nil, is consulted by persistLocked before it writes.
// It is a test-only seam for injecting a transient persist failure (to prove
// that one does not permanently poison the ledger). Production never sets it.
var persistHook func() error

func (l *Ledger) persistLocked() error {
	if persistHook != nil {
		if err := persistHook(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeUnavailable, "write request-key ledger", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "create request-key ledger directory", err)
	}
	disk := ledgerFileData{Version: ledgerVersion, Entries: make([]*KeyEntry, 0, len(l.order))}
	for _, key := range l.order {
		disk.Entries = append(disk.Entries, l.entries[key])
	}
	data, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "encode request-key ledger", err)
	}
	tmp := fmt.Sprintf("%s.tmp-%d", l.path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "create request-key ledger temp file", err)
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "write request-key ledger", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "sync request-key ledger", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "close request-key ledger", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "replace request-key ledger", err)
	}
	dir, err := os.Open(filepath.Dir(l.path))
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "open request-key ledger directory", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "sync request-key ledger directory", err)
	}
	return nil
}

// indeterminateResult builds the terminal envelope for an entry whose
// execution was interrupted by a process restart: the outcome is unknown and
// the mutation is never re-executed under that key (fail-closed). The message
// tells the consumer how to proceed — inspect the operation, or use a new key
// once the outcome has been established by other means.
func indeterminateResult(e *KeyEntry) b64Bytes {
	env := &machine.Envelope{
		SchemaVersion: machine.SchemaVersion,
		OperationID:   e.OperationID,
		Status:        machine.StatusFailed,
		Error: &machine.ErrorBody{
			Code: phelixerr.CodeUnavailable.String(),
			Message: "process restarted while this request key was in progress; the outcome is indeterminate and the operation was not re-executed — " +
				"inspect the operation state, then retry under a fresh request key (or, if this was a plan apply, create and apply a new plan)",
			ExitCode: 1,
		},
	}
	data, err := machine.MarshalEnvelope(env)
	if err != nil { // cannot fail for this fixed shape, but stay total
		return b64Bytes(`{"schema_version":"1","status":"failed","error":{"code":"UNAVAILABLE","message":"outcome indeterminate","exit_code":1}}`)
	}
	return data
}

func kindOr(kind string) string {
	if kind == "" {
		return "operation"
	}
	return kind
}

func appOr(app string) string {
	if app == "" {
		return "an unknown app"
	}
	return app
}

// operationRef names the operation an entry is linked to, for error messages.
func operationRef(e *KeyEntry) string {
	if e.OperationID == "" {
		return "operation id not yet recorded"
	}
	return e.OperationID
}
