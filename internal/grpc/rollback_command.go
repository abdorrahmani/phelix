package grpc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/protobuf/proto"
)

const (
	rollbackLedgerVersion  = 1
	rollbackLedgerCapacity = 256
	rollbackLedgerFile     = "remote-rollback-ledger.json"
	ledgerStateInProgress  = "in_progress"
	ledgerStateComplete    = "complete"
)

// rollbackLedgerEntry is one durably-recorded remote command. Command records
// the command type ("rollback", "matrix_build", …) so an agent restart can
// build an accurate indeterminate result for an entry whose execution was cut
// short; entries written before the field existed (rollback-only ledgers)
// fall back to the ledger's fallbackCommand.
type rollbackLedgerEntry struct {
	RequestID   string                   `json:"request_id"`
	Fingerprint string                   `json:"fingerprint"`
	Command     string                   `json:"command,omitempty"`
	State       string                   `json:"state"`
	Result      *pb.MonitorCommandResult `json:"result,omitempty"`
	Delivered   bool                     `json:"delivered,omitempty"`
	CreatedAt   int64                    `json:"created_at"`
	UpdatedAt   int64                    `json:"updated_at"`
}

type rollbackLedgerFileData struct {
	Version int                    `json:"version"`
	Entries []*rollbackLedgerEntry `json:"entries"`
}

// rollbackLedger is the durable idempotency ledger for remote commands. It is
// generic over the command type: rollback and matrix commands each own one
// instance (separate files, separate failure domains) with the same
// begin/complete/markDelivered lifecycle.
type rollbackLedger struct {
	mu      sync.Mutex
	path    string
	max     int
	entries map[string]*rollbackLedgerEntry
	order   []string
	pending map[string]bool
	ready   bool
	err     error
	// fallbackCommand labels in-progress entries that predate the per-entry
	// Command field (legacy rollback ledgers).
	fallbackCommand string
	// indeterminateNote extends the restart message with command-type-specific
	// guidance (e.g. where a matrix run's state lives).
	indeterminateNote string
}

// The remote rollback transport compiled into this build (this ledger plus
// the rollback dispatch in monitor_stream.go) declares its capability.
func init() { RegisterCapability(CapabilityRollback) }

var rollbackResults = newRollbackLedger(filepath.Join(server.DataDir(), rollbackLedgerFile), rollbackLedgerCapacity, "rollback", "")

func newRollbackLedger(path string, max int, fallbackCommand, indeterminateNote string) *rollbackLedger {
	return &rollbackLedger{
		path:              path,
		max:               max,
		entries:           make(map[string]*rollbackLedgerEntry),
		pending:           make(map[string]bool),
		fallbackCommand:   fallbackCommand,
		indeterminateNote: indeterminateNote,
	}
}

// InitializeRollbackLedger loads the durable remote-rollback idempotency
// ledger. Monitor startup must call it before accepting commands. It is safe to
// call repeatedly for the same data directory. A corrupt or unreadable existing
// ledger returns an error and leaves command handling fail-closed. The path is
// re-resolved from the current data directory on every call so a test or
// daemon whose PHELIX_DATA_DIR/HOME changed since process start re-anchors the
// ledger instead of trusting a stale eager path.
func InitializeRollbackLedger() error {
	return rollbackResults.initializeAt(filepath.Join(server.DataDir(), rollbackLedgerFile))
}

// RollbackLedgerPath exposes the effective ledger location for startup
// diagnostics and integration tests.
func RollbackLedgerPath() string {
	rollbackResults.mu.Lock()
	defer rollbackResults.mu.Unlock()
	return rollbackResults.path
}

func (l *rollbackLedger) initializeAt(path string) error {
	l.mu.Lock()
	if l.path != path {
		l.path = path
		l.entries = make(map[string]*rollbackLedgerEntry)
		l.order = nil
		l.pending = make(map[string]bool)
		l.ready = false
		l.err = nil
	}
	l.mu.Unlock()
	return l.initialize()
}

func (l *rollbackLedger) initialize() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ready || l.err != nil {
		return l.err
	}
	if err := l.loadLocked(); err != nil {
		l.err = err
		return err
	}
	changed := false
	now := time.Now().UnixMilli()
	for _, id := range l.order {
		e := l.entries[id]
		if e.State != ledgerStateInProgress {
			continue
		}
		e.State = ledgerStateComplete
		e.UpdatedAt = now
		e.Result = l.indeterminateResult(e)
		e.Delivered = false
		l.pending[id] = true
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

func (l *rollbackLedger) loadLocked() error {
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "read remote rollback ledger", err)
	}
	var disk rollbackLedgerFileData
	if err := json.Unmarshal(data, &disk); err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "decode remote rollback ledger", err)
	}
	if disk.Version != rollbackLedgerVersion {
		return phelixerr.Newf(phelixerr.CodeUnavailable, "unsupported remote rollback ledger version %d", disk.Version)
	}
	if len(disk.Entries) > l.max {
		return phelixerr.Newf(phelixerr.CodeUnavailable, "remote rollback ledger exceeds capacity %d", l.max)
	}
	for _, e := range disk.Entries {
		if e == nil || e.RequestID == "" || e.Fingerprint == "" || (e.State != ledgerStateInProgress && e.State != ledgerStateComplete) {
			return phelixerr.New(phelixerr.CodeUnavailable, "remote rollback ledger contains an invalid entry")
		}
		if e.State == ledgerStateComplete && e.Result == nil {
			return phelixerr.New(phelixerr.CodeUnavailable, "remote rollback ledger completed entry has no result")
		}
		if _, exists := l.entries[e.RequestID]; exists {
			return phelixerr.New(phelixerr.CodeUnavailable, "remote rollback ledger contains duplicate request_id")
		}
		l.entries[e.RequestID] = e
		l.order = append(l.order, e.RequestID)
		if e.State == ledgerStateComplete && !e.Delivered {
			l.pending[e.RequestID] = true
		}
	}
	return nil
}

func rollbackRequestFingerprint(req *pb.MonitorCommandRequest) (string, error) {
	if req == nil {
		return "", phelixerr.New(phelixerr.CodeInvalidArgument, "rollback command is nil")
	}
	canonical := proto.Clone(req).(*pb.MonitorCommandRequest)
	canonical.RequestId = ""
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(canonical)
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeUnavailable, "fingerprint rollback command", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

type rollbackBeginOutcome int

const (
	rollbackBeginExecute rollbackBeginOutcome = iota
	rollbackBeginReplay
)

func (l *rollbackLedger) begin(req *pb.MonitorCommandRequest) (rollbackBeginOutcome, *pb.MonitorCommandResult, error) {
	fingerprint, err := rollbackRequestFingerprint(req)
	if err != nil {
		return 0, nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ready || l.err != nil {
		return 0, nil, l.unavailableLocked()
	}
	if existing := l.entries[req.GetRequestId()]; existing != nil {
		if existing.Fingerprint != fingerprint {
			return 0, nil, phelixerr.New(phelixerr.CodeAlreadyExists,
				"request_id already exists with a different "+l.commandNoun(existing)+" payload")
		}
		if existing.State == ledgerStateInProgress {
			return 0, nil, phelixerr.Newf(phelixerr.CodeUnavailable,
				"%s request is already in progress", l.commandNoun(existing))
		}
		return rollbackBeginReplay, cloneMonitorCommandResult(existing.Result), nil
	}
	if len(l.entries) >= l.max {
		return 0, nil, phelixerr.Newf(phelixerr.CodeUnavailable, "remote rollback ledger capacity %d reached", l.max)
	}
	now := time.Now().UnixMilli()
	entry := &rollbackLedgerEntry{
		RequestID:   req.GetRequestId(),
		Fingerprint: fingerprint,
		Command:     req.GetType(),
		State:       ledgerStateInProgress,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	l.entries[entry.RequestID] = entry
	l.order = append(l.order, entry.RequestID)
	if err := l.persistLocked(); err != nil {
		delete(l.entries, entry.RequestID)
		l.order = l.order[:len(l.order)-1]
		l.err = err
		return 0, nil, err
	}
	return rollbackBeginExecute, nil, nil
}

func (l *rollbackLedger) complete(requestID string, result *pb.MonitorCommandResult) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ready || l.err != nil {
		return l.unavailableLocked()
	}
	e := l.entries[requestID]
	if e == nil || e.State != ledgerStateInProgress {
		return phelixerr.New(phelixerr.CodeUnavailable, "remote rollback ledger has no in-progress request")
	}
	e.State = ledgerStateComplete
	e.Result = cloneMonitorCommandResult(result)
	e.Delivered = false
	e.UpdatedAt = time.Now().UnixMilli()
	l.pending[requestID] = true
	if err := l.persistLocked(); err != nil {
		l.err = err
		return err
	}
	return nil
}

func (l *rollbackLedger) pendingResults() ([]*pb.MonitorCommandResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ready || l.err != nil {
		return nil, l.unavailableLocked()
	}
	out := make([]*pb.MonitorCommandResult, 0, len(l.pending))
	for _, id := range l.order {
		if l.pending[id] {
			out = append(out, cloneMonitorCommandResult(l.entries[id].Result))
		}
	}
	return out, nil
}

func (l *rollbackLedger) markDelivered(requestID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[requestID]
	if e == nil || e.State != ledgerStateComplete {
		return phelixerr.New(phelixerr.CodeUnavailable, "remote rollback ledger has no completed request")
	}
	if e.Delivered {
		delete(l.pending, requestID)
		return nil
	}
	e.Delivered = true
	if err := l.persistLocked(); err != nil {
		e.Delivered = false
		l.err = err
		return err
	}
	delete(l.pending, requestID)
	return nil
}

// commandNoun names the command an entry belongs to for error messages
// ("rollback", "matrix_build", …). Legacy entries predate the per-entry
// Command field and fall back to the ledger's fallbackCommand.
func (l *rollbackLedger) commandNoun(e *rollbackLedgerEntry) string {
	if e.Command != "" {
		return e.Command
	}
	return l.fallbackCommand
}

func (l *rollbackLedger) unavailableLocked() error {
	if l.err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "remote rollback ledger unavailable", l.err)
	}
	return phelixerr.New(phelixerr.CodeUnavailable, "remote rollback ledger is not initialized")
}

func (l *rollbackLedger) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "create remote rollback ledger directory", err)
	}
	disk := rollbackLedgerFileData{Version: rollbackLedgerVersion, Entries: make([]*rollbackLedgerEntry, 0, len(l.order))}
	for _, id := range l.order {
		disk.Entries = append(disk.Entries, l.entries[id])
	}
	data, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "encode remote rollback ledger", err)
	}
	tmp := fmt.Sprintf("%s.tmp-%d", l.path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "create remote rollback ledger temp file", err)
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "write remote rollback ledger", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "sync remote rollback ledger", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "close remote rollback ledger", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "secure remote rollback ledger", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "replace remote rollback ledger", err)
	}
	dir, err := os.Open(filepath.Dir(l.path))
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "open remote rollback ledger directory", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "sync remote rollback ledger directory", err)
	}
	return nil
}

// indeterminateResult builds the terminal result for an entry whose
// execution was interrupted by an agent restart: the outcome is unknown and
// the command is never re-executed (fail-closed). The message carries the
// ledger's command-type-specific follow-up guidance.
func (l *rollbackLedger) indeterminateResult(e *rollbackLedgerEntry) *pb.MonitorCommandResult {
	command := e.Command
	if command == "" {
		command = l.fallbackCommand
	}
	message := "agent restarted while the command was in progress; outcome is indeterminate and the command was not re-executed"
	if l.indeterminateNote != "" {
		message += " — " + l.indeterminateNote
	}
	return &pb.MonitorCommandResult{
		RequestId: e.RequestID,
		Command:   command,
		Status:    "error",
		Error:     message,
		ErrorCode: string(phelixerr.CodeUnavailable),
		Timestamp: time.Now().UnixMilli(),
	}
}
