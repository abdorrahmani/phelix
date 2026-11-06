package security

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PrefetchSource is the scan-time advisory source: it batch-queries OSV
// for the package identities a scan will match, fills the advisory cache,
// and then serves matching purely from the cache. When the network is
// unavailable it falls back to a full index of whatever the cache already
// holds, and reports that the data may be stale.
type PrefetchSource struct {
	cache  *AdvisoryCache
	client *OSVClient
	Now    func() time.Time

	mu             sync.Mutex
	byQuery        map[string][]string
	fetchFailures  int
	staleUsed      bool
	networkFailure bool
}

// NewPrefetchSource builds the advisory source over a cache and client.
func NewPrefetchSource(cache *AdvisoryCache, client *OSVClient) *PrefetchSource {
	return &PrefetchSource{
		cache:   cache,
		client:  client,
		Now:     time.Now,
		byQuery: map[string][]string{},
	}
}

// Prefetch resolves advisory IDs for every query and loads missing or
// expired advisories into the cache with bounded concurrency.
func (s *PrefetchSource) Prefetch(ctx context.Context, queries []OSVQuery) error {
	if len(queries) > 0 {
		ids, err := s.client.QueryIDs(ctx, queries)
		if err == nil {
			for i, query := range queries {
				s.setIDs(query, ids[i])
			}
			s.refreshDetails(ctx)
			return nil
		}
		s.mu.Lock()
		s.networkFailure = true
		s.mu.Unlock()
	}
	s.indexCacheFallback()
	return nil
}

// refreshDetails loads missing or expired advisories for the recorded
// queries, bounded by the detail concurrency limit.
func (s *PrefetchSource) refreshDetails(ctx context.Context) {
	seen := map[string]bool{}
	var missing []string
	for _, ids := range s.snapshotQueries() {
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			adv, stale, err := s.cache.Get(id)
			switch {
			case err != nil:
				continue
			case adv == nil, stale:
				missing = append(missing, id)
			}
		}
	}

	if len(missing) == 0 {
		return
	}
	sem := make(chan struct{}, osvDetailConcurrency)
	var wg sync.WaitGroup
	for _, id := range missing {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			adv, err := s.client.FetchAdvisory(ctx, id)
			if err != nil {
				s.mu.Lock()
				s.fetchFailures++
				s.mu.Unlock()
				return
			}
			s.cache.Put(adv, s.Now())
		}(id)
	}
	wg.Wait()
}

// indexCacheFallback builds query keys from every cache entry when the
// batch query is unreachable, so a repeat scan can run offline on data it
// already holds.
func (s *PrefetchSource) indexCacheFallback() {
	entries := listCacheAdvisories(s.cache.dir)
	for _, adv := range entries {
		for _, affected := range adv.Affected {
			eco := osvEcosystemFromName(affected.Package.Ecosystem)
			if eco == "" {
				continue
			}
			key := queryKey(eco, affected.Package.Name)
			s.mu.Lock()
			s.byQuery[key] = appendUnique(s.byQuery[key], adv.ID)
			s.mu.Unlock()
		}
	}
}

// Advisories serves the advisories recorded for one package identity,
// entirely from the cache. Stale entries are served as explicitly stale.
func (s *PrefetchSource) Advisories(ctx context.Context, eco Ecosystem, name string) ([]AdvisoryEntry, error) {
	ids := s.ids(OSVQuery{Ecosystem: eco, Name: name})
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]AdvisoryEntry, 0, len(ids))
	for _, id := range ids {
		adv, stale, err := s.cache.Get(id)
		if err != nil {
			return nil, err
		}
		if adv == nil {
			s.mu.Lock()
			s.fetchFailures++
			s.mu.Unlock()
			continue
		}
		if stale {
			s.mu.Lock()
			s.staleUsed = true
			s.mu.Unlock()
		}
		out = append(out, AdvisoryEntry{Advisory: adv, Stale: stale, FetchedAt: s.cacheFetchedAt(id)})
	}
	return out, nil
}

// StaleUsed reports whether any served advisory was past its freshness
// window.
func (s *PrefetchSource) StaleUsed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.staleUsed
}

// NetworkFailed reports whether the batch query could not reach OSV.
func (s *PrefetchSource) NetworkFailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.networkFailure
}

// FetchFailures counts advisories that could not be loaded.
func (s *PrefetchSource) FetchFailures() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetchFailures
}

// HasData reports whether any advisory identity is resolvable, either from
// the live query or from the cache fallback. A scan with no resolvable
// data must fail rather than report an empty result as clean.
func (s *PrefetchSource) HasData() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byQuery) > 0
}

func (s *PrefetchSource) ids(query OSVQuery) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byQuery[queryKey(query.Ecosystem, query.Name)]
}

func (s *PrefetchSource) setIDs(query OSVQuery, ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byQuery[queryKey(query.Ecosystem, query.Name)] = ids
}

func (s *PrefetchSource) snapshotQueries() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, 0, len(s.byQuery))
	for _, ids := range s.byQuery {
		out = append(out, ids)
	}
	return out
}

// cacheFetchedAt reads one entry's fetch timestamp for provenance.
func (s *PrefetchSource) cacheFetchedAt(id string) time.Time {
	raw, err := os.ReadFile(filepath.Join(s.cache.dir, id+".json"))
	if err != nil {
		return time.Time{}
	}
	var entry advisoryCacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return time.Time{}
	}
	return entry.FetchedAt
}

// listCacheAdvisories loads every parseable advisory currently cached.
// Corrupt entries are skipped; they are never trusted.
func listCacheAdvisories(dir string) []*OSVAdvisory {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []*OSVAdvisory
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var cached advisoryCacheEntry
		if err := json.Unmarshal(raw, &cached); err != nil {
			continue
		}
		if cached.Advisory == nil || cached.Advisory.ID == "" {
			continue
		}
		out = append(out, cached.Advisory)
	}
	return out
}

// osvEcosystemFromName maps an OSV ecosystem string back to the internal
// value.
func osvEcosystemFromName(name string) Ecosystem {
	switch name {
	case "Go":
		return EcosystemGo
	case "Cargo":
		return EcosystemRust
	default:
		return ""
	}
}
