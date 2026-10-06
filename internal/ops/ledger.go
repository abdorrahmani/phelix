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

	entryStateInProgress = "in_progress"
	entryStateComplete   = "complete"
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
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
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
	dir     string
	path    string
	max     int
	entries map[string]*KeyEntry
	order   []string
	ready   bool
	err     error
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
//     executing it), or the ledger is corrupt/over capacity (fail-closed).
func BeginKey(key, fingerprint, kind, app string) (BeginOutcome, *KeyEntry, error) {
	if key == "" {
		return BeginExecute, nil, phelixerr.New(phelixerr.CodeInvalidArgument, "empty request key")
	}
	keyLedger.mu.Lock()
	defer keyLedger.mu.Unlock()
	return keyLedger.beginLocked(key, fingerprint, kind, app)
}

// beginLocked implements the begin semantics with the mutex already held.
// Callers must run initializeLocked first (BeginKey and tests do).
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
	l.evictLocked()
	now := time.Now().UnixMilli()
	entry := &KeyEntry{
		Key:         key,
		Fingerprint: fingerprint,
		Kind:        kind,
		App:         app,
		State:       entryStateInProgress,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	l.entries[key] = entry
	l.order = append(l.order, key)
	if err := l.persistLocked(); err != nil {
		delete(l.entries, key)
		l.order = l.order[:len(l.order)-1]
		l.err = err
		return 0, nil, err
	}
	return BeginExecute, entry, nil
}

// CompleteKeyData records the terminal envelope for a key. data is the exact
// byte string the fresh execution printed (machine.MarshalEnvelope output)
// and is replayed byte-for-byte on repeated requests, whether the operation
// succeeded or failed — a consumed key never re-executes.
func CompleteKeyData(key, operationID string, data []byte) error {
	if key == "" || len(data) == 0 {
		return nil
	}
	keyLedger.mu.Lock()
	defer keyLedger.mu.Unlock()
	return keyLedger.completeLocked(key, operationID, data)
}

// completeLocked implements the complete semantics with the mutex already
// held and the envelope already marshaled.
func (l *Ledger) completeLocked(key, operationID string, data []byte) error {
	if !l.ready || l.err != nil {
		return l.unavailableLocked()
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
		l.err = err
		return err
	}
	return nil
}

// evictLocked drops the oldest completed entries to make room. In-progress
// entries are never evicted — that would let a key execute twice.
func (l *Ledger) evictLocked() {
	for len(l.entries) >= l.max {
		evicted := false
		for i, key := range l.order {
			e := l.entries[key]
			if e == nil || e.State != entryStateComplete {
				continue
			}
			delete(l.entries, key)
			l.order = append(l.order[:i], l.order[i+1:]...)
			evicted = true
			break
		}
		if !evicted {
			return
		}
	}
}

// initializeLocked loads the ledger once per process. Mirroring the
// remote-command ledger, an unreadable, undecodable, wrong-version,
// over-capacity, or invalid ledger poisons the instance: every later
// begin/complete answers UNAVAILABLE (fail-closed) instead of executing
// without idempotency. Entries left in_progress by a restart are closed as
// indeterminate — never re-executed.
func (l *Ledger) initializeLocked() error {
	// Re-anchor when the effective data directory changed since the last
	// use (test isolation, PHELIX_DATA_DIR overrides). A directory switch
	// resets the in-memory view so the new location is authoritative.
	if dir := server.DataDir(); dir != l.dir {
		l.ready = false
		l.entries = make(map[string]*KeyEntry)
		l.order = nil
		l.err = nil
		l.dir = dir
	}
	l.path = filepath.Join(l.dir, "ops", ledgerFile)
	if l.ready || l.err != nil {
		return l.err
	}
	if err := l.loadLocked(); err != nil {
		l.err = err
		return err
	}
	changed := false
	now := time.Now().UnixMilli()
	for _, key := range l.order {
		e := l.entries[key]
		if e.State != entryStateInProgress {
			continue
		}
		e.State = entryStateComplete
		e.UpdatedAt = now
		e.Result = indeterminateResult(e)
		changed = true
	}
	if changed {
		if err := l.persistLocked(); err != nil {
			l.err = err
			return err
		}
	}
	l.ready = true
	return nil
}

func (l *Ledger) loadLocked() error {
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
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "decode request-key ledger", err)
	}
	if disk.Version != ledgerVersion {
		return phelixerr.Newf(phelixerr.CodeUnavailable, "unsupported request-key ledger version %d", disk.Version)
	}
	if len(disk.Entries) > l.max {
		return phelixerr.Newf(phelixerr.CodeUnavailable, "request-key ledger exceeds capacity %d", l.max)
	}
	for _, e := range disk.Entries {
		if e == nil || e.Key == "" || e.Fingerprint == "" || (e.State != entryStateInProgress && e.State != entryStateComplete) {
			return phelixerr.New(phelixerr.CodeUnavailable, "request-key ledger contains an invalid entry")
		}
		if e.State == entryStateComplete && len(e.Result) == 0 {
			return phelixerr.New(phelixerr.CodeUnavailable, "request-key ledger completed entry has no result")
		}
		if _, exists := l.entries[e.Key]; exists {
			return phelixerr.New(phelixerr.CodeUnavailable, "request-key ledger contains duplicate key")
		}
		l.entries[e.Key] = e
		l.order = append(l.order, e.Key)
	}
	return nil
}

func (l *Ledger) persistLocked() error {
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

func (l *Ledger) unavailableLocked() error {
	if l.err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "request-key ledger unavailable", l.err)
	}
	return phelixerr.New(phelixerr.CodeUnavailable, "request-key ledger is not initialized")
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
				"inspect the operation state, then use a new request key if a retry is truly safe",
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
