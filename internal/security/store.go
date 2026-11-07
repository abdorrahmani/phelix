package security

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/version"
)

// scanSchemaVersion is the on-disk scan record version. Foreign versions
// are rejected rather than interpreted.
const scanSchemaVersion = 1

// DefaultScanRetention bounds how many scan records are kept per app.
const DefaultScanRetention = 50

// MaxStoredFindings bounds how many open findings one scan record keeps.
// The overflow is reported as a warning, never silently dropped.
const MaxStoredFindings = 1000

var scanIDPattern = regexp.MustCompile(`^scn_[0-9a-f]{16}$`)

// NewScanID returns a fresh scan identifier.
func NewScanID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("security: cannot generate scan id: %v", err))
	}
	return "scn_" + hex.EncodeToString(buf[:])
}

func validScanID(id string) bool { return scanIDPattern.MatchString(id) }

// ScanStatus is the lifecycle of one scan execution.
type ScanStatus string

const (
	ScanRunning     ScanStatus = "running"
	ScanCompleted   ScanStatus = "completed"
	ScanFailed      ScanStatus = "failed"
	ScanInterrupted ScanStatus = "interrupted"
)

// ScanTrigger records what started the scan.
type ScanTrigger string

const (
	TriggerManual    ScanTrigger = "manual"
	TriggerScheduled ScanTrigger = "scheduled"
)

// Advisory freshness and coverage values reported by a scan.
const (
	FreshnessFresh = "fresh"
	FreshnessStale = "stale"

	// CoverageComplete: every queried package was checked against
	// advisory data.
	CoverageComplete = "complete"
	// CoveragePartial: some packages could not be checked because no
	// advisory data was reachable for them. A partial scan is never a
	// clean bill of health.
	CoveragePartial = "partial"
)

// ScanRecord is one scan execution and its results. The record stores file
// fingerprints, never source paths: local filesystem layout is not copied
// into persisted state.
type ScanRecord struct {
	SchemaVersion int               `json:"schema_version"`
	ScanID        string            `json:"scan_id"`
	AppID         string            `json:"app_id,omitempty"`
	AppName       string            `json:"app_name"`
	IdentityKey   string            `json:"identity_key"`
	Trigger       ScanTrigger       `json:"trigger"`
	Status        ScanStatus        `json:"status"`
	StartedAt     time.Time         `json:"started_at"`
	FinishedAt    *time.Time        `json:"finished_at,omitempty"`
	SourceFiles   map[string]string `json:"source_files,omitempty"`
	Inventory     *Inventory        `json:"inventory,omitempty"`
	Findings      []Finding         `json:"findings,omitempty"`
	Counts        ScanCounts        `json:"counts"`
	Freshness     string            `json:"advisory_freshness"`
	Coverage      string            `json:"advisory_coverage,omitempty"`
	Warnings      []string          `json:"warnings,omitempty"`
	Outcome       string            `json:"outcome,omitempty"`
	PhelixVersion string            `json:"phelix_version,omitempty"`
	ScheduledFor  *time.Time        `json:"scheduled_for,omitempty"`
}

// ScanCounts summarizes one scan. Open findings are affected,
// potentially_affected, and unknown; not_affected is evaluated but not
// stored.
type ScanCounts struct {
	Modules             int            `json:"modules"`
	Matchable           int            `json:"matchable"`
	Evaluated           int            `json:"evaluated_advisories"`
	Affected            int            `json:"affected"`
	PotentiallyAffected int            `json:"potentially_affected"`
	Unknown             int            `json:"unknown"`
	NotAffected         int            `json:"not_affected"`
	BySeverity          map[string]int `json:"by_severity,omitempty"`
}

// Open reports whether the counts contain any finding at or above the
// severity threshold. Unknown severities follow the documented threshold
// policy: they never trip the gate on their own, and reports always show
// them.
func (c ScanCounts) AtOrAbove(threshold string) (bool, int) {
	minimum := SeverityRank(threshold)
	total := 0
	for severity, count := range c.BySeverity {
		if SeverityRank(severity) >= minimum && SeverityRank(severity) > 0 {
			total += count
		}
	}
	return total > 0, total
}

// ScanStore persists scan records, the latest index, and advisory cache
// entries under <dataDir>/security.
type ScanStore struct {
	root string
	// Now makes the clock injectable for tests.
	Now func() time.Time
}

// NewScanStore returns the scan store rooted at dataDir.
func NewScanStore(dataDir string) *ScanStore {
	return &ScanStore{root: filepath.Join(dataDir, "security"), Now: time.Now}
}

func (s *ScanStore) scansDir() string   { return filepath.Join(s.root, "scans") }
func (s *ScanStore) locksDir() string   { return filepath.Join(s.root, "locks") }
func (s *ScanStore) cacheDir() string   { return filepath.Join(s.root, "advisory-cache") }
func (s *ScanStore) latestPath() string { return filepath.Join(s.root, "latest.json") }

func (s *ScanStore) ensureDirs() error {
	for _, dir := range []string{s.scansDir(), s.locksDir(), s.cacheDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "security: create %s", dir)
		}
	}
	return nil
}

// AdvisoryCacheDir is where the scan store keeps advisory cache entries.
func (s *ScanStore) AdvisoryCacheDir() string { return s.cacheDir() }

// SaveScan writes a scan record atomically.
func (s *ScanStore) SaveScan(record *ScanRecord) error {
	if record == nil || !validScanID(record.ScanID) {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "cannot save a scan record without a valid scan id")
	}
	record.SchemaVersion = scanSchemaVersion
	if record.PhelixVersion == "" {
		record.PhelixVersion = version.Version
	}
	if err := s.ensureDirs(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "could not encode scan record", err)
	}
	return writeFileAtomic(s.scanPath(record.ScanID), data, 0o600)
}

func (s *ScanStore) scanPath(scanID string) string {
	return filepath.Join(s.scansDir(), scanID+".json")
}

// LoadScan reads one scan record. Corrupt or foreign records are
// quarantined and reported missing, never trusted.
func (s *ScanStore) LoadScan(scanID string) (*ScanRecord, error) {
	if !validScanID(scanID) {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "invalid scan id %q", scanID)
	}
	raw, err := os.ReadFile(s.scanPath(scanID))
	if os.IsNotExist(err) {
		return nil, phelixerr.Newf(phelixerr.CodeNotFound, "no scan record %s", scanID)
	}
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read scan record %s", scanID)
	}
	var record ScanRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		_ = quarantineCacheEntry(s.scanPath(scanID))
		return nil, phelixerr.Newf(phelixerr.CodeFilesystem, "scan record %s is corrupt and was quarantined", scanID)
	}
	if record.SchemaVersion != scanSchemaVersion {
		_ = quarantineCacheEntry(s.scanPath(scanID))
		return nil, phelixerr.Newf(phelixerr.CodeFilesystem, "scan record %s has unsupported schema version %d", scanID, record.SchemaVersion)
	}
	return &record, nil
}

// ListScans returns every stored scan record for one identity, newest
// first. Records that fail validation are skipped and counted.
func (s *ScanStore) ListScans(identityKey string) ([]*ScanRecord, error) {
	entries, err := os.ReadDir(s.scansDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "security: list scans")
	}
	var out []*ScanRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		scanID := strings.TrimSuffix(entry.Name(), ".json")
		record, err := s.LoadScan(scanID)
		if err != nil {
			continue
		}
		if record.IdentityKey == identityKey {
			out = append(out, record)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// Retain keeps the newest keep scans for one identity and removes the
// rest, never touching a running record.
func (s *ScanStore) Retain(identityKey string, keep int) error {
	records, err := s.ListScans(identityKey)
	if err != nil {
		return err
	}
	for i, record := range records {
		if i < keep || record.Status == ScanRunning {
			continue
		}
		os.Remove(s.scanPath(record.ScanID))
	}
	return nil
}

// LatestEntry is one app's row in the latest index: the newest completed
// scan and the next scheduled scan time.
type LatestEntry struct {
	ScanID     string     `json:"scan_id,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	NextScanAt *time.Time `json:"next_scan_at,omitempty"`
}

type latestIndex struct {
	SchemaVersion int                    `json:"schema_version"`
	Apps          map[string]LatestEntry `json:"apps"`
}

// LatestIndex loads the latest index. A corrupt file is quarantined and
// treated as empty: scan records remain the durable truth.
func (s *ScanStore) loadLatestIndex() (latestIndex, error) {
	raw, err := os.ReadFile(s.latestPath())
	if os.IsNotExist(err) {
		return latestIndex{SchemaVersion: scanSchemaVersion, Apps: map[string]LatestEntry{}}, nil
	}
	if err != nil {
		return latestIndex{}, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "security: read latest index")
	}
	var index latestIndex
	if err := json.Unmarshal(raw, &index); err != nil {
		_ = quarantineCacheEntry(s.latestPath())
		return latestIndex{SchemaVersion: scanSchemaVersion, Apps: map[string]LatestEntry{}}, nil
	}
	if index.Apps == nil {
		index.Apps = map[string]LatestEntry{}
	}
	return index, nil
}

func (s *ScanStore) saveLatestIndex(index latestIndex) error {
	if err := s.ensureDirs(); err != nil {
		return err
	}
	index.SchemaVersion = scanSchemaVersion
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "could not encode latest index", err)
	}
	return writeFileAtomic(s.latestPath(), data, 0o600)
}

// withLatestLock runs fn while holding the latest index lock so concurrent
// CLI and daemon processes cannot lose each other's updates.
func (s *ScanStore) withLatestLock(fn func(index latestIndex) (latestIndex, error)) error {
	if err := s.ensureDirs(); err != nil {
		return err
	}
	lock, err := acquireLock(filepath.Join(s.locksDir(), "latest.lock"), true)
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	defer lock.Close()
	index, err := s.loadLatestIndex()
	if err != nil {
		return err
	}
	updated, err := fn(index)
	if err != nil {
		return err
	}
	return s.saveLatestIndex(updated)
}

// MarkLatestSuccess records a completed scan as the app's latest result.
// The next scheduled scan time is owned by the scheduler through
// ScheduleNext and is deliberately left untouched here.
func (s *ScanStore) MarkLatestSuccess(identityKey string, scanID string, finishedAt time.Time) error {
	return s.withLatestLock(func(index latestIndex) (latestIndex, error) {
		entry := index.Apps[identityKey]
		entry.ScanID = scanID
		when := finishedAt
		entry.FinishedAt = &when
		index.Apps[identityKey] = entry
		return index, nil
	})
}

// ScheduleNext updates only the next scheduled scan time, preserving the
// last successful scan pointer. Failed scans keep the last good report.
func (s *ScanStore) ScheduleNext(identityKey string, nextScanAt time.Time) error {
	return s.withLatestLock(func(index latestIndex) (latestIndex, error) {
		entry := index.Apps[identityKey]
		when := nextScanAt
		entry.NextScanAt = &when
		index.Apps[identityKey] = entry
		return index, nil
	})
}

// Latest returns the latest entry for one identity. When the index has no
// row, the newest completed scan record for the identity is used so a
// corrupt or lost index never hides scan history.
func (s *ScanStore) Latest(identityKey string) (*LatestEntry, error) {
	index, err := s.loadLatestIndex()
	if err != nil {
		return nil, err
	}
	if entry, ok := index.Apps[identityKey]; ok {
		return &entry, nil
	}
	records, err := s.ListScans(identityKey)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.Status == ScanCompleted {
			finished := *record.FinishedAt
			return &LatestEntry{ScanID: record.ScanID, FinishedAt: &finished}, nil
		}
	}
	return nil, nil
}

// ErrScanLockHeld reports that another process holds the per-app scan
// lock. Callers implementing a scheduling policy treat it as "skip this
// round", never as a failure.
var ErrScanLockHeld = errors.New("scan lock is held by another process")

// ScanLock exclusively locks one app identity for the duration of a scan.
type ScanLock struct {
	file *os.File
}

// AcquireScanLock takes the exclusive per-app scan lock. Manual and
// scheduled scans coordinate through the same lock.
func (s *ScanStore) AcquireScanLock(identityKey string) (*ScanLock, error) {
	if err := s.ensureDirs(); err != nil {
		return nil, err
	}
	lock, err := acquireLock(filepath.Join(s.locksDir(), lockFileName(identityKey)), true)
	if err != nil {
		return nil, err
	}
	return &ScanLock{file: lock}, nil
}

// TryAcquireScanLock takes the per-app scan lock without blocking. When a
// manual scan holds the lock the call fails with ErrScanLockHeld.
func (s *ScanStore) TryAcquireScanLock(identityKey string) (*ScanLock, error) {
	if err := s.ensureDirs(); err != nil {
		return nil, err
	}
	lock, err := acquireLockTry(filepath.Join(s.locksDir(), lockFileName(identityKey)))
	if err != nil {
		return nil, err
	}
	return &ScanLock{file: lock}, nil
}

// Release drops the scan lock.
func (l *ScanLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	releaseLock(l.file)
	l.file.Close()
	l.file = nil
}

// lockFileName maps an identity key to a safe lock file name.
func lockFileName(identityKey string) string {
	var out []rune
	for _, r := range identityKey {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	name := string(out)
	if len(name) > 64 {
		name = name[:64]
	}
	return name + ".lock"
}

// FingerprintFiles hashes the dependency files present in dir and returns
// a file name to SHA-256 map.
func FingerprintFiles(dir string, names []string) map[string]string {
	out := map[string]string{}
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sum := sha256.Sum256(raw)
		out[name] = hex.EncodeToString(sum[:])
	}
	return out
}
