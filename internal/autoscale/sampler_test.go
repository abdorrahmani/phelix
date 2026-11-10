package autoscale

import (
	"context"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

type fakeReader struct {
	busy map[int]float64
	rss  map[int]uint64
	errs map[int]bool
	dead map[int]bool
}

func (f *fakeReader) busySeconds(pid int) (float64, error) {
	if f.errs[pid] {
		return 0, context.DeadlineExceeded
	}
	return f.busy[pid], nil
}

func (f *fakeReader) rssBytes(pid int) (uint64, error) {
	if f.errs[pid] {
		return 0, context.DeadlineExceeded
	}
	return f.rss[pid], nil
}

func (f *fakeReader) alive(pid int) (bool, error) {
	if f.dead[pid] {
		return false, nil
	}
	return true, nil
}

type fakeStats struct {
	stats    []proxy.BackendStat
	statsErr error
	status   []proxy.AppStatus
}

func (f *fakeStats) Stats(ctx context.Context, app string) ([]proxy.BackendStat, error) {
	return f.stats, f.statsErr
}

func (f *fakeStats) Status(ctx context.Context, app string) ([]proxy.AppStatus, error) {
	return f.status, nil
}

func newTestSampler(t *testing.T, app string, stats ProxyStatsSource, reader procReader) (*Sampler, *time.Time) {
	t.Helper()
	base := time.Unix(1700000000, 0)
	s := NewSampler(app, stats)
	s.now = func() time.Time { return base }
	s.reader = reader
	s.hostMemoryBytes = func() (uint64, error) { return 8 << 30, nil }
	s.SetMemoryLimit(8 << 30)
	return s, &base
}

func TestSamplerCPUDeltaAndMemory(t *testing.T) {
	r := &fakeReader{busy: map[int]float64{101: 10.0}, rss: map[int]uint64{101: 1 << 30}}
	s, base := newTestSampler(t, "api", nil, r)

	snap := s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}})
	if !snap.Metrics.Missing.Has(deploy.MissingCPU) {
		t.Errorf("first observation should flag CPU missing: %+v", snap.Metrics)
	}
	if snap.Metrics.MemoryPercent == 0 {
		t.Errorf("memory percent should be computed on the first pass: %+v", snap.Metrics)
	}

	r.busy[101] = 20.0
	*base = base.Add(10 * time.Second)
	s.now = func() time.Time { return *base }
	snap = s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}})
	if snap.Metrics.CPUPercent != 100 {
		t.Errorf("CPU = %.2f, want 100 (1 core-second over 10s)", snap.Metrics.CPUPercent)
	}
	if snap.Metrics.MemoryPercent != 12.5 {
		t.Errorf("memory = %.2f, want 12.5 (1GiB of 8GiB)", snap.Metrics.MemoryPercent)
	}
	if snap.Metrics.Missing != 0 {
		t.Errorf("missing = %v, want none", snap.Metrics.Missing)
	}
}

func TestSamplerMaxAcrossReplicas(t *testing.T) {
	r := &fakeReader{
		busy: map[int]float64{101: 0.0, 102: 0.0},
		rss:  map[int]uint64{101: 1 << 30, 102: 2 << 30},
	}
	s, base := newTestSampler(t, "api", nil, r)
	s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}, {Key: "1", PID: 102}})

	r.busy[101] = 1.0
	r.busy[102] = 3.0
	*base = base.Add(10 * time.Second)
	s.now = func() time.Time { return *base }
	snap := s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}, {Key: "1", PID: 102}})
	if snap.Metrics.CPUPercent != 30 {
		t.Errorf("CPU = %.2f, want 30 (max of 10 and 30)", snap.Metrics.CPUPercent)
	}
	if snap.Metrics.MemoryPercent != 25 {
		t.Errorf("memory = %.2f, want 25 (max of 12.5 and 25)", snap.Metrics.MemoryPercent)
	}
	if len(snap.Replicas) != 2 {
		t.Errorf("replica samples = %d, want 2", len(snap.Replicas))
	}
}

func TestSamplerDeadReplicaBlocksDown(t *testing.T) {
	r := &fakeReader{busy: map[int]float64{101: 0.0}, rss: map[int]uint64{101: 1 << 30}, errs: map[int]bool{}}
	s, base := newTestSampler(t, "api", nil, r)
	s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}})

	r.busy[101] = 1.0
	r.errs[102] = true
	r.busy[102] = 0
	*base = base.Add(10 * time.Second)
	s.now = func() time.Time { return *base }
	snap := s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}, {Key: "1", PID: 102}})
	if !snap.Metrics.Missing.Has(deploy.MissingCPU) {
		t.Errorf("a dead replica should flag CPU missing: %+v", snap.Metrics)
	}
	if !snap.Metrics.Missing.Has(deploy.MissingMemory) {
		t.Errorf("a dead replica should flag memory missing: %+v", snap.Metrics)
	}
}

func TestSamplerTrafficWindows(t *testing.T) {
	mk := func(req int64, buckets [11]uint64) proxy.BackendStat {
		return proxy.BackendStat{Label: "replica-0", Host: "127.0.0.1:9001", Requests: req, Buckets: buckets[:]}
	}
	fs := &fakeStats{stats: []proxy.BackendStat{mk(100, [11]uint64{60, 90, 100, 100, 100, 100, 100, 100, 100, 100, 100})}}
	s, base := newTestSampler(t, "api", fs, &fakeReader{})

	snap := s.Collect(context.Background(), nil)
	if !snap.Traffic.Applicable || snap.Traffic.Fresh {
		t.Errorf("first window = %+v, want applicable but not fresh", snap.Traffic)
	}
	if !snap.Metrics.Missing.Has(deploy.MissingRPS) || !snap.Metrics.Missing.Has(deploy.MissingLatency) {
		t.Errorf("first window should flag traffic missing: %+v", snap.Metrics)
	}

	fs.stats = []proxy.BackendStat{mk(160, [11]uint64{70, 120, 160, 160, 160, 160, 160, 160, 160, 160, 160})}
	*base = base.Add(10 * time.Second)
	s.now = func() time.Time { return *base }
	snap = s.Collect(context.Background(), nil)
	if !snap.Traffic.Fresh {
		t.Fatalf("second window should be fresh: %+v", snap.Traffic)
	}
	if snap.Traffic.Requests != 60 {
		t.Errorf("requests = %d, want 60", snap.Traffic.Requests)
	}
	if snap.Traffic.RPS != 6 {
		t.Errorf("rps = %.2f, want 6", snap.Traffic.RPS)
	}
	if snap.Metrics.RPS != 6 {
		t.Errorf("metrics rps = %.2f, want 6", snap.Metrics.RPS)
	}
	if snap.Traffic.P95 <= 0 || snap.Traffic.P95 > 25*time.Millisecond {
		t.Errorf("p95 = %v, want within the 25ms bound", snap.Traffic.P95)
	}
	if snap.Metrics.P95Latency != snap.Traffic.P95 {
		t.Errorf("metrics p95 = %v, want %v", snap.Metrics.P95Latency, snap.Traffic.P95)
	}
}

func TestSamplerTrafficProxyRestartClampsNegativeDeltas(t *testing.T) {
	mk := func(req int64) proxy.BackendStat {
		return proxy.BackendStat{Label: "replica-0", Host: "127.0.0.1:9001", Requests: req}
	}
	fs := &fakeStats{stats: []proxy.BackendStat{mk(500)}}
	s, base := newTestSampler(t, "api", fs, &fakeReader{})
	s.Collect(context.Background(), nil)

	fs.stats = []proxy.BackendStat{mk(40)}
	*base = base.Add(10 * time.Second)
	s.now = func() time.Time { return *base }
	snap := s.Collect(context.Background(), nil)
	if snap.Traffic.Requests != 0 || snap.Traffic.RPS != 0 {
		t.Errorf("post-restart window = %+v, want clamped to zero", snap.Traffic)
	}
	if !snap.Traffic.Fresh {
		t.Errorf("post-restart window should still be fresh (non-negative): %+v", snap.Traffic)
	}
}

func TestSamplerTrafficLossIsMissingNotIdle(t *testing.T) {
	fs := &fakeStats{stats: []proxy.BackendStat{{Label: "replica-0", Host: "127.0.0.1:9001", Requests: 10}}}
	s, base := newTestSampler(t, "api", fs, &fakeReader{})
	s.Collect(context.Background(), nil)

	fs.statsErr = context.DeadlineExceeded
	*base = base.Add(10 * time.Second)
	s.now = func() time.Time { return *base }
	snap := s.Collect(context.Background(), nil)
	if !snap.Traffic.Applicable {
		t.Errorf("a previously enrolled app stays applicable: %+v", snap.Traffic)
	}
	if snap.Traffic.Fresh {
		t.Errorf("a failed scrape is not a fresh window: %+v", snap.Traffic)
	}
	if !snap.Metrics.Missing.Has(deploy.MissingRPS) || !snap.Metrics.Missing.Has(deploy.MissingLatency) {
		t.Errorf("failed traffic scrape should flag missing: %+v", snap.Metrics)
	}

	fs.statsErr = nil
	fs.stats = nil
	*base = base.Add(10 * time.Second)
	snap = s.Collect(context.Background(), nil)
	if !snap.Traffic.Applicable {
		t.Errorf("empty stats after enrollment stay applicable: %+v", snap.Traffic)
	}
	if !snap.Metrics.Missing.Has(deploy.MissingRPS) {
		t.Errorf("empty stats should flag missing rather than read as zero: %+v", snap.Metrics)
	}
}

func TestSamplerWorkerWithoutProxy(t *testing.T) {
	s, base := newTestSampler(t, "jobs", nil, &fakeReader{busy: map[int]float64{7: 0}, rss: map[int]uint64{7: 1 << 20}})
	s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 7}})

	*base = base.Add(5 * time.Second)
	s.now = func() time.Time { return *base }
	snap := s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 7}})
	if snap.Traffic.Applicable {
		t.Errorf("worker traffic should not be applicable: %+v", snap.Traffic)
	}
	if snap.Metrics.TrafficApplicable {
		t.Errorf("metrics should mark traffic not applicable: %+v", snap.Metrics)
	}
	if snap.Metrics.Missing.Has(deploy.MissingRPS) {
		t.Errorf("not-applicable traffic is not missing data: %+v", snap.Metrics)
	}
}

func TestSamplerZeroCPUIsAvailable(t *testing.T) {
	r := &fakeReader{busy: map[int]float64{101: 5.0}, rss: map[int]uint64{101: 1 << 20}}
	s, base := newTestSampler(t, "api", nil, r)
	s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}})

	*base = base.Add(10 * time.Second)
	s.now = func() time.Time { return *base }
	snap := s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}})
	if snap.Replicas[0].CPUState != MetricAvailable {
		t.Errorf("CPU state = %s, want available", snap.Replicas[0].CPUState)
	}
	if snap.Replicas[0].CPUPercent != 0 {
		t.Errorf("CPU = %.2f, want 0", snap.Replicas[0].CPUPercent)
	}
	if snap.Metrics.Missing.Has(deploy.MissingCPU) {
		t.Errorf("an idle process is measured, not missing: %+v", snap.Metrics)
	}
}

func TestSamplerMemoryWithoutLimitIsUnavailable(t *testing.T) {
	r := &fakeReader{busy: map[int]float64{101: 0}, rss: map[int]uint64{101: 2 << 30}}
	s, base := newTestSampler(t, "api", nil, r)
	s.SetMemoryLimit(0)
	s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}})

	*base = base.Add(10 * time.Second)
	s.now = func() time.Time { return *base }
	snap := s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}})
	rs := snap.Replicas[0]
	if rs.MemoryState != MetricAvailable || rs.MemoryBytes != 2<<30 {
		t.Errorf("memory = %+v, want available 2GiB", rs)
	}
	if rs.MemoryPctState != MetricUnavailable || rs.MemoryPercent != 0 {
		t.Errorf("memory percent = %.2f/%s, want unavailable without a limit", rs.MemoryPercent, rs.MemoryPctState)
	}
	if rs.HostMemState != MetricAvailable || rs.HostMemoryPct != 25 {
		t.Errorf("host memory = %.2f/%s, want available 25 percent of 8GiB", rs.HostMemoryPct, rs.HostMemState)
	}
	if !snap.Metrics.Missing.Has(deploy.MissingMemory) {
		t.Errorf("autoscaling memory should be missing without a limit: %+v", snap.Metrics)
	}
}

func TestSamplerProcessExited(t *testing.T) {
	r := &fakeReader{dead: map[int]bool{102: true}}
	s, _ := newTestSampler(t, "api", nil, r)
	snap := s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 102, Status: "running"}})
	rs := snap.Replicas[0]
	if rs.ProcessStatus != "exited" {
		t.Errorf("process status = %q, want exited", rs.ProcessStatus)
	}
	for name, state := range map[string]MetricState{
		"cpu": rs.CPUState, "memory": rs.MemoryState, "memory percent": rs.MemoryPctState, "host memory": rs.HostMemState,
	} {
		if state != MetricProcessExited {
			t.Errorf("%s state = %s, want process_exited", name, state)
		}
	}
	if !snap.Metrics.Missing.Has(deploy.MissingCPU) || !snap.Metrics.Missing.Has(deploy.MissingMemory) {
		t.Errorf("exited replica should flag both dimensions missing: %+v", snap.Metrics)
	}
}

func TestSamplerStaleThenUnavailable(t *testing.T) {
	r := &fakeReader{busy: map[int]float64{101: 5.0}, rss: map[int]uint64{101: 1 << 30}, errs: map[int]bool{}}
	s, base := newTestSampler(t, "api", nil, r)
	s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}})
	*base = base.Add(10 * time.Second)
	s.now = func() time.Time { return *base }
	s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}})

	r.errs[101] = true
	*base = base.Add(10 * time.Second)
	s.now = func() time.Time { return *base }
	rs := s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}}).Replicas[0]
	if rs.CPUState != MetricStale || rs.MemoryState != MetricStale {
		t.Errorf("within freshness = cpu %s, memory %s, want stale", rs.CPUState, rs.MemoryState)
	}
	if rs.MemoryBytes != 1<<30 {
		t.Errorf("stale memory = %d, want last value", rs.MemoryBytes)
	}

	*base = base.Add(2 * DefaultFreshnessWindow)
	s.now = func() time.Time { return *base }
	rs = s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}}).Replicas[0]
	if rs.CPUState != MetricUnavailable || rs.MemoryState != MetricUnavailable {
		t.Errorf("past freshness = cpu %s, memory %s, want unavailable", rs.CPUState, rs.MemoryState)
	}
	if rs.MemoryBytes != 0 {
		t.Errorf("unavailable memory = %d, want no value", rs.MemoryBytes)
	}

	r.errs[101] = false
	*base = base.Add(10 * time.Second)
	s.now = func() time.Time { return *base }
	rs = s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}}).Replicas[0]
	if rs.CPUState != MetricInitializing {
		t.Errorf("CPU after a read failure = %s, want initializing", rs.CPUState)
	}
	*base = base.Add(10 * time.Second)
	s.now = func() time.Time { return *base }
	rs = s.Collect(context.Background(), []ReplicaRef{{Key: "0", PID: 101}}).Replicas[0]
	if rs.CPUState != MetricAvailable || rs.CPUPercent != 0 {
		t.Errorf("CPU after recovery = %.2f/%s, want 0/available", rs.CPUPercent, rs.CPUState)
	}
}

func TestReplicaRefsDeterministicSlotOrder(t *testing.T) {
	state := &deploy.DeployState{Replicas: map[string]*deploy.Instance{
		"10": {PID: 10, Status: "running"},
		"2":  {PID: 2, Status: "running"},
		"1":  {PID: 1, Status: "stopped"},
	}}
	refs := ReplicaRefs(state)
	if len(refs) != 3 || refs[0].Key != "1" || refs[1].Key != "2" || refs[2].Key != "10" {
		t.Errorf("refs = %+v, want numeric slot order", refs)
	}
	if ReplicaRefs(nil) != nil {
		t.Errorf("nil state should yield nil refs")
	}
}
