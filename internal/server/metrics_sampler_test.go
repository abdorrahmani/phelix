package server

import (
	"testing"
	"time"
)

// waitForCachedSample polls until the background sampler has produced its
// first snapshot, or the deadline passes.
func waitForCachedSample(t *testing.T) *Metrics {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		metricsCacheMu.RLock()
		cached := metricsCache
		metricsCacheMu.RUnlock()
		if cached != nil {
			return cached
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("sampler did not produce a sample within 2s")
	return nil
}

// TestCollectMetricsIsNonBlocking pins the property the background sampler
// exists for: reading metrics must never measure the host for a fixed
// interval. The pre-sampler implementation called cpu.Percent(time.Second),
// which blocked every caller for a full second and forced the monitor's
// server-metrics tick down to 30s.
func TestCollectMetricsIsNonBlocking(t *testing.T) {
	start := time.Now()
	m, err := CollectMetrics()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("CollectMetrics returned an error: %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("CollectMetrics blocked for %v; reads must come from the sampler cache or an incremental sample", elapsed)
	}
	if m == nil {
		t.Fatal("CollectMetrics returned nil metrics")
	}
	if m.CPUUsagePercent < 0 || m.CPUUsagePercent > 100 {
		t.Fatalf("CPU usage out of range: %v", m.CPUUsagePercent)
	}
}

// TestMetricsSamplerKeepsSnapshotFresh starts the sampler at a fast interval
// and checks that successive reads observe fresh snapshots, i.e. the cache
// really is being refreshed rather than served once and frozen.
func TestMetricsSamplerKeepsSnapshotFresh(t *testing.T) {
	StartMetricsSampler(20 * time.Millisecond)

	waitForCachedSample(t)

	first, err := CollectMetrics()
	if err != nil {
		t.Fatalf("CollectMetrics returned an error: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		next, err := CollectMetrics()
		if err != nil {
			t.Fatalf("CollectMetrics returned an error: %v", err)
		}
		if next.Timestamp.After(first.Timestamp) {
			return // cache refreshed since the first read
		}
	}
	t.Fatal("sampler cache never refreshed within 2s")
}
