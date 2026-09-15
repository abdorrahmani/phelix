package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// DeliveryLedgerFile is the ledger file name inside <dataDir>/webhook/.
const DeliveryLedgerFile = "deliveries.json"

// DefaultLedgerCapacity bounds the dedup window. Delivery IDs are kept in
// insertion order and the oldest entries are evicted first — a redelivery
// arrives within seconds of the original, so a bounded recent window is the
// correct replay protection and the file cannot grow without limit.
const DefaultLedgerCapacity = 512

const deliveryLedgerVersion = 1

// deliveryRecord is one durably accepted webhook delivery.
type deliveryRecord struct {
	Key        string `json:"key"` // app + "\x00" + delivery id
	App        string `json:"app"`
	DeliveryID string `json:"delivery_id"`
	Branch     string `json:"branch"`
	Commit     string `json:"commit"`
	Provider   string `json:"provider"`
	ReceivedAt int64  `json:"received_at"`
}

type ledgerFileData struct {
	Version int               `json:"version"`
	Entries []*deliveryRecord `json:"entries"`
}

// DeliveryLedger is the durable idempotency ledger for webhook deliveries: the
// same delivery ID must never enqueue a second rebuild, across concurrent
// requests and daemon restarts. It follows the same durability discipline as
// the remote-command ledgers (atomic tmp+rename with fsync, fail-closed on
// corrupt state): a delivery is recorded before the request is acknowledged,
// so a crash after the write still deduplicates a redelivery.
type DeliveryLedger struct {
	mu      sync.Mutex
	path    string
	max     int
	entries map[string]*deliveryRecord
	order   []string
	ready   bool
	err     error
}

// NewDeliveryLedger creates a ledger backed by path. Initialize must be called
// (and succeed) before the ledger accepts deliveries.
func NewDeliveryLedger(path string, capacity int) *DeliveryLedger {
	return &DeliveryLedger{
		path:    path,
		max:     capacity,
		entries: make(map[string]*deliveryRecord),
	}
}

// Initialize loads the ledger from disk. A corrupt or unreadable file fails
// closed (the webhook server must refuse to start without replay protection)
// rather than silently losing the dedup window.
func (l *DeliveryLedger) Initialize() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ready {
		return nil
	}
	if err := l.loadLocked(); err != nil {
		l.err = err
		return err
	}
	l.ready = true
	return nil
}

func (l *DeliveryLedger) loadLocked() error {
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "read webhook delivery ledger", err)
	}
	var disk ledgerFileData
	if err := json.Unmarshal(data, &disk); err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "decode webhook delivery ledger", err)
	}
	if disk.Version != deliveryLedgerVersion {
		return phelixerr.Newf(phelixerr.CodeUnavailable, "unsupported webhook delivery ledger version %d", disk.Version)
	}
	for _, e := range disk.Entries {
		if e == nil || e.Key == "" || e.App == "" || e.DeliveryID == "" {
			return phelixerr.New(phelixerr.CodeUnavailable, "webhook delivery ledger contains an invalid entry")
		}
		if _, exists := l.entries[e.Key]; exists {
			return phelixerr.New(phelixerr.CodeUnavailable, "webhook delivery ledger contains a duplicate entry")
		}
		l.entries[e.Key] = e
		l.order = append(l.order, e.Key)
	}
	return nil
}

func deliveryKey(app, deliveryID string) string {
	return app + "\x00" + deliveryID
}

// SeenOrRecord atomically checks whether app/deliveryID was already accepted
// and, if not, durably records it. seen=true means the delivery was processed
// before (this request must not enqueue another rebuild). The whole
// check-and-record runs under the ledger mutex, so concurrent duplicate
// deliveries produce exactly one winner.
func (l *DeliveryLedger) SeenOrRecord(app, deliveryID, branch, commit, provider string) (seen bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ready || l.err != nil {
		return false, l.unavailableLocked()
	}
	key := deliveryKey(app, deliveryID)
	if _, exists := l.entries[key]; exists {
		return true, nil
	}
	rec := &deliveryRecord{
		Key:        key,
		App:        app,
		DeliveryID: deliveryID,
		Branch:     branch,
		Commit:     commit,
		Provider:   provider,
		ReceivedAt: time.Now().UnixMilli(),
	}
	l.entries[key] = rec
	l.order = append(l.order, key)
	l.evictLocked()
	if err := l.persistLocked(); err != nil {
		// Roll the in-memory insert back so a retry of the same delivery can
		// be recorded once the filesystem recovers.
		delete(l.entries, key)
		l.order = l.order[:len(l.order)-1]
		l.err = err
		return false, err
	}
	return false, nil
}

// Remove undoes a record when the delivery was accepted but could not be
// enqueued (e.g. the per-app queue was full and the request was rejected
// with 503). The provider will redeliver, and that redelivery must start from
// a clean slate instead of being deduplicated against a job that never ran.
func (l *DeliveryLedger) Remove(app, deliveryID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ready || l.err != nil {
		return l.unavailableLocked()
	}
	key := deliveryKey(app, deliveryID)
	if _, exists := l.entries[key]; !exists {
		return nil
	}
	delete(l.entries, key)
	for i, k := range l.order {
		if k == key {
			l.order = append(l.order[:i], l.order[i+1:]...)
			break
		}
	}
	if err := l.persistLocked(); err != nil {
		l.err = err
		return err
	}
	return nil
}

// Len reports the number of recorded deliveries (diagnostics and tests).
func (l *DeliveryLedger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// DeliveryInfo is the read-only, wire-safe view of one recorded delivery:
// metadata only — never the request body, signature or headers.
type DeliveryInfo struct {
	App        string
	DeliveryID string
	Branch     string
	Commit     string
	Provider   string
	ReceivedAt int64
}

// RecentForApp returns the app's recorded deliveries, newest first, up to
// limit (0 = all retained). Read-only: it never modifies dedup state.
func (l *DeliveryLedger) RecentForApp(app string, limit int) []DeliveryInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []DeliveryInfo
	// l.order is insertion order; scan backwards for newest-first.
	for i := len(l.order) - 1; i >= 0; i-- {
		e := l.entries[l.order[i]]
		if e == nil || e.App != app {
			continue
		}
		out = append(out, DeliveryInfo{
			App:        e.App,
			DeliveryID: e.DeliveryID,
			Branch:     e.Branch,
			Commit:     e.Commit,
			Provider:   e.Provider,
			ReceivedAt: e.ReceivedAt,
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// evictLocked drops the oldest entries above capacity.
func (l *DeliveryLedger) evictLocked() {
	for len(l.order) > l.max {
		oldest := l.order[0]
		l.order = l.order[1:]
		delete(l.entries, oldest)
	}
}

func (l *DeliveryLedger) unavailableLocked() error {
	if l.err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "webhook delivery ledger unavailable", l.err)
	}
	return phelixerr.New(phelixerr.CodeUnavailable, "webhook delivery ledger is not initialized")
}

// persistLocked atomically writes the ledger (tmp file + fsync + rename +
// directory fsync), the same discipline as the remote-command ledgers.
func (l *DeliveryLedger) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "create webhook delivery ledger directory", err)
	}
	disk := ledgerFileData{Version: deliveryLedgerVersion, Entries: make([]*deliveryRecord, 0, len(l.order))}
	for _, key := range l.order {
		disk.Entries = append(disk.Entries, l.entries[key])
	}
	data, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "encode webhook delivery ledger", err)
	}
	tmp := fmt.Sprintf("%s.tmp-%d", l.path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "create webhook delivery ledger temp file", err)
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "write webhook delivery ledger", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "sync webhook delivery ledger", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "close webhook delivery ledger", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "replace webhook delivery ledger", err)
	}
	dir, err := os.Open(filepath.Dir(l.path))
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "open webhook delivery ledger directory", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeUnavailable, "sync webhook delivery ledger directory", err)
	}
	return nil
}
