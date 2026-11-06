package security

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// AdvisoryCacheTTL bounds how long a fetched advisory is served without a
// refresh. AdvisoryCachePruneAge bounds how long expired entries stay on
// disk for offline scans.
const (
	AdvisoryCacheTTL      = 24 * time.Hour
	AdvisoryCachePruneAge = 7 * 24 * time.Hour
)

// cacheEntrySchemaVersion is bumped when the on-disk cache entry shape
// changes. Foreign versions are rejected, never interpreted.
const cacheEntrySchemaVersion = 1

// AdvisoryCache stores fetched advisories as one JSON file per OSV ID
// under a directory. Entries are validated on every load: corrupt or
// foreign entries are quarantined and treated as absent, never trusted.
type AdvisoryCache struct {
	dir string
	// Now makes the clock injectable for tests.
	Now func() time.Time
}

// NewAdvisoryCache returns a cache rooted at dir.
func NewAdvisoryCache(dir string) *AdvisoryCache {
	return &AdvisoryCache{dir: dir, Now: time.Now}
}

type advisoryCacheEntry struct {
	SchemaVersion int          `json:"schema_version"`
	ID            string       `json:"id"`
	FetchedAt     time.Time    `json:"fetched_at"`
	Advisory      *OSVAdvisory `json:"advisory"`
}

// Get returns the advisory for id. The second result reports whether the
// entry is present but past its freshness window: such entries are still
// usable as explicitly stale data when the network is unreachable.
func (c *AdvisoryCache) Get(id string) (*OSVAdvisory, bool, error) {
	if !validOSVID(id) {
		return nil, false, phelixerr.Newf(phelixerr.CodeInvalidArgument, "invalid OSV advisory id %q", id)
	}
	path := c.path(id)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read advisory cache entry %s", id)
	}
	var entry advisoryCacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		if qerr := quarantineCacheEntry(path); qerr != nil {
			return nil, false, qerr
		}
		return nil, false, nil
	}
	if entry.SchemaVersion != cacheEntrySchemaVersion || entry.Advisory == nil || entry.Advisory.ID != id || entry.FetchedAt.IsZero() {
		if qerr := quarantineCacheEntry(path); qerr != nil {
			return nil, false, qerr
		}
		return nil, false, nil
	}
	return entry.Advisory, c.Now().Sub(entry.FetchedAt) > AdvisoryCacheTTL, nil
}

// Put stores an advisory with the given fetch time.
func (c *AdvisoryCache) Put(advisory *OSVAdvisory, fetchedAt time.Time) error {
	if advisory == nil || !validOSVID(advisory.ID) {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "cannot cache an advisory without a valid id")
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "security: create advisory cache %s", c.dir)
	}
	entry := advisoryCacheEntry{
		SchemaVersion: cacheEntrySchemaVersion,
		ID:            advisory.ID,
		FetchedAt:     fetchedAt.UTC(),
		Advisory:      advisory,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "could not encode advisory cache entry", err)
	}
	path := c.path(advisory.ID)
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return err
	}
	return os.Chtimes(path, fetchedAt, fetchedAt)
}

// Prune removes cache entries whose fetch time is older than maxAge.
func (c *AdvisoryCache) Prune(maxAge time.Duration) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	cutoff := c.Now().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() || !stringsHasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		os.Remove(filepath.Join(c.dir, entry.Name()))
	}
}

func (c *AdvisoryCache) path(id string) string {
	return filepath.Join(c.dir, id+".json")
}

// quarantineCacheEntry renames an unusable entry out of the cache so the
// next fetch starts clean and nothing downstream can trust the bytes.
func quarantineCacheEntry(path string) error {
	return os.Rename(path, path+".corrupt."+time.Now().UTC().Format("20060102T150405"))
}

func stringsHasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
