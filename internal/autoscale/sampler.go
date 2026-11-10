// Package autoscale hosts the automatic autoscaling controller: a per-app
// metrics sampler, the pause/history persistence and the evaluation loop
// that turns the deploy package's decision engine into real scaling
// operations. It adds no daemon: the controller runs inside the existing
// monitor process, and every replica mutation goes through the existing
// scaling paths.
package autoscale

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/process"
)

// DefaultFreshnessWindow is the age after which a per-replica sample is no
// longer considered a live measurement. The autoscaling controller uses the
// same rule scaled by its evaluation interval (twice the interval), so
// presentation and decisions share one definition.
const DefaultFreshnessWindow = 30 * time.Second

// FreshnessWindow returns the accepted sample age for one evaluation cadence.
// A non-positive interval falls back to the documented default.
func FreshnessWindow(interval time.Duration) time.Duration {
	if interval <= 0 {
		return DefaultFreshnessWindow
	}
	return 2 * interval
}

// MetricState describes whether one collected value is trustworthy. A zero
// value is only reported as available when the process really used no CPU or
// memory; missing data is never rendered as zero.
type MetricState string

const (
	MetricAvailable     MetricState = "available"
	MetricInitializing  MetricState = "initializing"
	MetricUnavailable   MetricState = "unavailable"
	MetricStale         MetricState = "stale"
	MetricProcessExited MetricState = "process_exited"
)

// ProxyStatsSource is the slice of the proxy control client the sampler
// needs. *proxy.Client satisfies it.
type ProxyStatsSource interface {
	Stats(ctx context.Context, appName string) ([]proxy.BackendStat, error)
	Status(ctx context.Context, appName string) ([]proxy.AppStatus, error)
}

// ReplicaRef identifies one replica to sample: the deploy.json slot key, its
// process ID and the recorded lifecycle status.
type ReplicaRef struct {
	Key    string
	PID    int
	Status string
}

// ReplicaSample is one replica's collected resource usage and the validity of
// each dimension. CPU is normalised to one logical core: a process using two
// full cores reports 200 percent. Memory percent is relative to the app's
// configured memory limit; host memory is exposed separately and never
// presented as an application limit.
type ReplicaSample struct {
	Key            string
	PID            int
	ProcessStatus  string
	CPUPercent     float64
	CPUState       MetricState
	MemoryBytes    uint64
	MemoryState    MetricState
	MemoryLimit    uint64
	MemoryPercent  float64
	MemoryPctState MetricState
	HostMemoryPct  float64
	HostMemState   MetricState
	At             time.Time
	Error          string
}

// TrafficWindow is one windowed observation of an app's proxy traffic,
// computed by diffing the cumulative backend counters between two samples.
type TrafficWindow struct {
	// Applicable reports whether proxy traffic metrics exist for this app at
	// all (a proxy is enrolled). When false the window is empty and the
	// engine excludes traffic metrics instead of reading them as zero.
	Applicable bool
	// Fresh reports whether a full observation window existed. The very
	// first collection after startup has no previous counters to diff
	// against and reports Fresh=false.
	Fresh     bool
	Requests  int64
	Errors    int64
	RPS       float64
	ErrorRate float64
	P95       time.Duration
	P99       time.Duration
	At        time.Time
}

// Snapshot is one collection pass: the engine-facing metrics plus the
// per-replica detail behind them.
type Snapshot struct {
	App      string
	At       time.Time
	Metrics  deploy.AutoscaleMetrics
	Replicas []ReplicaSample
	Traffic  TrafficWindow
}

// procSample is the retained CPU-time state for one PID between two
// collections.
type procSample struct {
	total float64
	at    time.Time
}

// lastSample is the last successful observation retained for presentation
// when a later read fails inside the freshness window.
type lastSample struct {
	cpu       float64
	cpuAt     time.Time
	memory    uint64
	memoryAt  time.Time
	hostPct   float64
	hostAt    time.Time
	hasCPU    bool
	hasMemory bool
	hasHost   bool
}

// procReader abstracts per-PID stats collection so tests can feed
// deterministic values.
type procReader interface {
	// busySeconds returns the process's cumulative user+system CPU time.
	busySeconds(pid int) (float64, error)
	// rssBytes returns the process's resident set size.
	rssBytes(pid int) (uint64, error)
	// alive reports whether the PID still exists.
	alive(pid int) (bool, error)
}

type gopsutilReader struct{}

func (gopsutilReader) busySeconds(pid int) (float64, error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, err
	}
	t, err := p.Times()
	if err != nil {
		return 0, err
	}
	return t.User + t.System, nil
}

func (gopsutilReader) rssBytes(pid int) (uint64, error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, err
	}
	m, err := p.MemoryInfo()
	if err != nil {
		return 0, err
	}
	return m.RSS, nil
}

func (gopsutilReader) alive(pid int) (bool, error) {
	ok, err := process.PidExists(int32(pid))
	if err != nil {
		return false, err
	}
	return ok, nil
}

// Sampler collects per-replica CPU and memory plus windowed proxy traffic
// for one app. It retains the previous CPU-time and traffic-counter state
// needed to turn cumulative counters into interval deltas, so one instance
// must be reused per app across collections.
type Sampler struct {
	app   string
	stats ProxyStatsSource

	// now, reader, hostMemoryBytes and memoryLimitBytes are test seams;
	// production leaves them at their defaults.
	now             func() time.Time
	reader          procReader
	hostMemoryBytes func() (uint64, error)

	mu          sync.Mutex
	cpuPrev     map[int]procSample
	last        map[int]lastSample
	memoryLimit uint64
	freshness   time.Duration
	trafficPrev []proxy.BackendStat
	trafficAt   time.Time
	everApplied bool
}

// NewSampler builds a sampler for one app. stats may be nil for apps without
// proxy traffic (workers and pure resource readers); traffic is then
// permanently not applicable.
func NewSampler(app string, stats ProxyStatsSource) *Sampler {
	return &Sampler{
		app:             app,
		stats:           stats,
		now:             time.Now,
		reader:          gopsutilReader{},
		hostMemoryBytes: defaultHostMemoryBytes,
		freshness:       DefaultFreshnessWindow,
		last:            make(map[int]lastSample),
	}
}

func defaultHostMemoryBytes() (uint64, error) {
	vm, err := mem.VirtualMemory()
	if err != nil {
		return 0, err
	}
	return vm.Total, nil
}

// SetStatsSource swaps the traffic source, for controllers that resolve the
// app mode per evaluation (rolling apps get the proxy, workers nil).
func (s *Sampler) SetStatsSource(stats ProxyStatsSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats = stats
}

// SetMemoryLimit sets the app's configured per-replica memory ceiling in
// bytes. Zero means no limit, in which case memory percent is unavailable
// rather than host-relative.
func (s *Sampler) SetMemoryLimit(limit uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memoryLimit = limit
}

// SetFreshnessWindow overrides the accepted sample age for stale fallback.
func (s *Sampler) SetFreshnessWindow(window time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if window > 0 {
		s.freshness = window
	}
}

// MemoryLimitBytesForApp resolves the app's configured memory limit through
// the existing resource policy. Zero means unlimited.
func MemoryLimitBytesForApp(appName string) uint64 {
	cfg, err := app.LoadResources(appName)
	if err != nil {
		return 0
	}
	limits, err := cfg.Limits()
	if err != nil {
		return 0
	}
	return limits.MemoryBytes
}

// ReplicaRefs returns every recorded replica in deterministic slot order,
// including stopped slots: a stopped replica is reported as exited, never as
// a removed replica.
func ReplicaRefs(state *deploy.DeployState) []ReplicaRef {
	if state == nil {
		return nil
	}
	keys := make([]string, 0, len(state.Replicas))
	for key := range state.Replicas {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, aerr := parseSlot(keys[i])
		b, berr := parseSlot(keys[j])
		if aerr == nil && berr == nil {
			return a < b
		}
		return keys[i] < keys[j]
	})
	refs := make([]ReplicaRef, 0, len(keys))
	for _, key := range keys {
		inst := state.Replicas[key]
		if inst == nil {
			continue
		}
		refs = append(refs, ReplicaRef{Key: key, PID: inst.PID, Status: inst.Status})
	}
	return refs
}

func parseSlot(key string) (int, error) {
	return strconv.Atoi(key)
}

// ResourceRefs returns the instances whose resources should be presented for
// an app. Rolling and worker apps use their recorded replicas; a blue-green
// app has slots instead, and those are reported under their slot names.
func ResourceRefs(state *deploy.DeployState) []ReplicaRef {
	if state == nil {
		return nil
	}
	if len(state.Replicas) > 0 {
		return ReplicaRefs(state)
	}
	if state.Mode != deploy.ModeBlueGreen {
		return nil
	}
	refs := make([]ReplicaRef, 0, len(state.Slots))
	for _, slot := range []string{deploy.SlotBlue, deploy.SlotGreen} {
		if inst := state.Slots[slot]; inst != nil {
			refs = append(refs, ReplicaRef{Key: slot, PID: inst.PID, Status: inst.Status})
		}
	}
	return refs
}

// Collect takes one observation of the given live replicas and the app's
// proxy counters. Replicas whose process could not be read are skipped from
// the engine aggregate and flag the corresponding dimension missing at the
// app level: the engine never scales down on data it could not see.
func (s *Sampler) Collect(ctx context.Context, replicas []ReplicaRef) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	snap := Snapshot{App: s.app, At: now}
	snap.Metrics.At = now

	samples, stats := s.collectResourcesLocked(now, replicas)
	snap.Replicas = samples
	snap.Metrics.CPUPercent = stats.maxCPU
	snap.Metrics.MemoryPercent = stats.maxMemoryPercent
	if stats.cpuMissing {
		snap.Metrics.Missing |= deploy.MissingCPU
	}
	if stats.memoryMissing {
		snap.Metrics.Missing |= deploy.MissingMemory
	}

	snap.Traffic = s.collectTrafficLocked(ctx, now)
	snap.Metrics.TrafficApplicable = snap.Traffic.Applicable
	if snap.Traffic.Applicable && !snap.Traffic.Fresh {
		snap.Metrics.Missing |= deploy.MissingRPS | deploy.MissingLatency
	} else if snap.Traffic.Applicable {
		snap.Metrics.RPS = snap.Traffic.RPS
		snap.Metrics.P95Latency = snap.Traffic.P95
		snap.Metrics.P99Latency = snap.Traffic.P99
	}
	return snap
}

// CollectResources takes one resource-only observation. Monitoring and the
// CLI use it so per-replica visibility never depends on autoscaling being
// enabled and never scrapes proxy traffic.
func (s *Sampler) CollectResources(replicas []ReplicaRef) []ReplicaSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	samples, _ := s.collectResourcesLocked(s.now(), replicas)
	return samples
}

// SampleResources takes two resource-only observations separated by interval
// so a one-shot caller gets a real CPU delta instead of the initializing
// state. Periodic callers keep using CollectResources.
func (s *Sampler) SampleResources(replicas []ReplicaRef, interval time.Duration) []ReplicaSample {
	s.CollectResources(replicas)
	if interval > 0 {
		time.Sleep(interval)
	}
	return s.CollectResources(replicas)
}

type resourceAggregate struct {
	maxCPU           float64
	maxMemoryPercent float64
	cpuMissing       bool
	memoryMissing    bool
}

func (s *Sampler) collectResourcesLocked(now time.Time, replicas []ReplicaRef) ([]ReplicaSample, resourceAggregate) {
	hostTotal, hostErr := s.hostMemoryBytes()
	prevCPU := s.cpuPrev
	s.cpuPrev = make(map[int]procSample, len(replicas))
	if s.last == nil {
		s.last = make(map[int]lastSample)
	}
	nextLast := make(map[int]lastSample, len(replicas))

	var agg resourceAggregate
	samples := make([]ReplicaSample, 0, len(replicas))
	for _, r := range replicas {
		rs := ReplicaSample{Key: r.Key, PID: r.PID, At: now}
		last := s.last[r.PID]
		alive := r.PID > 0
		if r.PID > 0 {
			if ok, err := s.reader.alive(r.PID); err == nil {
				alive = ok
			}
		}
		switch {
		case !alive:
			rs.ProcessStatus = "exited"
			rs.CPUState = MetricProcessExited
			rs.MemoryState = MetricProcessExited
			rs.MemoryPctState = MetricProcessExited
			rs.HostMemState = MetricProcessExited
			agg.cpuMissing = true
			agg.memoryMissing = true
			samples = append(samples, rs)
			continue
		case r.Status != "":
			rs.ProcessStatus = r.Status
		default:
			rs.ProcessStatus = "unknown"
		}

		if total, err := s.reader.busySeconds(r.PID); err == nil {
			s.cpuPrev[r.PID] = procSample{total: total, at: now}
			if prev, ok := prevCPU[r.PID]; ok {
				elapsed := now.Sub(prev.at).Seconds()
				if elapsed > 0 {
					pct := (total - prev.total) / elapsed * 100
					if pct < 0 {
						pct = 0
					}
					rs.CPUPercent = pct
					rs.CPUState = MetricAvailable
					last.cpu, last.cpuAt, last.hasCPU = pct, now, true
					if pct > agg.maxCPU {
						agg.maxCPU = pct
					}
				} else {
					rs.CPUState = MetricInitializing
				}
			} else {
				// First observation of this PID: no interval to diff over,
				// so the CPU value is unavailable this round.
				rs.CPUState = MetricInitializing
			}
		} else {
			rs.CPUState = fallbackState(last.hasCPU, last.cpuAt, now, s.freshness)
			if rs.CPUState == MetricStale {
				rs.CPUPercent = last.cpu
			}
			rs.Error = err.Error()
		}
		if rs.CPUState != MetricAvailable {
			agg.cpuMissing = true
		}

		if rss, err := s.reader.rssBytes(r.PID); err == nil {
			rs.MemoryBytes = rss
			rs.MemoryState = MetricAvailable
			last.memory, last.memoryAt, last.hasMemory = rss, now, true
		} else {
			rs.MemoryState = fallbackState(last.hasMemory, last.memoryAt, now, s.freshness)
			if rs.MemoryState == MetricStale {
				rs.MemoryBytes = last.memory
			}
			if rs.Error == "" {
				rs.Error = err.Error()
			}
		}
		if rs.MemoryState != MetricAvailable {
			agg.memoryMissing = true
		}

		rs.MemoryLimit = s.memoryLimit
		if s.memoryLimit > 0 {
			if rs.MemoryState == MetricAvailable || rs.MemoryState == MetricStale {
				rs.MemoryPercent = float64(rs.MemoryBytes) / float64(s.memoryLimit) * 100
				rs.MemoryPctState = rs.MemoryState
				if rs.MemoryPctState == MetricAvailable && rs.MemoryPercent > agg.maxMemoryPercent {
					agg.maxMemoryPercent = rs.MemoryPercent
				}
			} else {
				rs.MemoryPctState = rs.MemoryState
			}
		} else {
			rs.MemoryPctState = MetricUnavailable
			agg.memoryMissing = true
		}

		if hostErr == nil && hostTotal > 0 {
			if rs.MemoryState == MetricAvailable || rs.MemoryState == MetricStale {
				rs.HostMemoryPct = float64(rs.MemoryBytes) / float64(hostTotal) * 100
				rs.HostMemState = rs.MemoryState
				last.hostPct, last.hostAt, last.hasHost = rs.HostMemoryPct, now, true
			} else {
				rs.HostMemState = rs.MemoryState
			}
		} else {
			rs.HostMemState = fallbackState(last.hasHost, last.hostAt, now, s.freshness)
			if rs.HostMemState == MetricStale {
				rs.HostMemoryPct = last.hostPct
			} else {
				rs.HostMemState = MetricUnavailable
			}
		}
		if rs.Error == "" && hostErr != nil {
			rs.Error = hostErr.Error()
		}

		nextLast[r.PID] = last
		samples = append(samples, rs)
	}
	s.last = nextLast
	return samples, agg
}

func fallbackState(has bool, at, now time.Time, freshness time.Duration) MetricState {
	if !has {
		return MetricUnavailable
	}
	if now.Sub(at) > freshness {
		return MetricUnavailable
	}
	return MetricStale
}

// collectTrafficLocked diffs the proxy's cumulative backend counters into
// one windowed observation. Negative deltas after a proxy daemon restart are
// clamped to zero, the same way rollout verification protects its windows.
func (s *Sampler) collectTrafficLocked(ctx context.Context, now time.Time) TrafficWindow {
	w := TrafficWindow{At: now}
	if s.stats == nil {
		return w
	}
	stats, err := s.stats.Stats(ctx, s.app)
	if err != nil || len(stats) == 0 {
		if s.everApplied {
			// The proxy was enrolled before; losing it now is missing data,
			// not the absence of traffic.
			w.Applicable = true
		}
		if err == nil {
			// Enrolled backends dropped to zero (every replica removed):
			// keep applicability but report no fresh window.
			if s.everApplied {
				w.Applicable = true
			}
			s.trafficPrev = nil
		}
		return w
	}
	w.Applicable = true
	if s.trafficPrev == nil {
		s.trafficPrev, s.trafficAt, s.everApplied = stats, now, true
		return w
	}
	elapsed := now.Sub(s.trafficAt).Seconds()
	if elapsed <= 0 {
		return w
	}
	prevByHost := make(map[string]proxy.BackendStat, len(s.trafficPrev))
	for _, b := range s.trafficPrev {
		prevByHost[b.Host] = b
	}
	var (
		deltaReq int64
		deltaErr int64
		buckets  = make([]uint64, len(proxy.LatencyBucketBounds))
	)
	for _, b := range stats {
		prev := prevByHost[b.Host]
		dReq := b.Requests - prev.Requests
		if dReq > 0 {
			deltaReq += dReq
		}
		dErr := b.Errors - prev.Errors
		if dErr > 0 {
			deltaErr += dErr
		}
		for i := range buckets {
			if i < len(b.Buckets) && i < len(prev.Buckets) {
				d := b.Buckets[i] - prev.Buckets[i]
				if d > 0 {
					buckets[i] += d
				}
			}
		}
	}
	w.Fresh = true
	w.Requests = deltaReq
	w.Errors = deltaErr
	w.RPS = float64(deltaReq) / elapsed
	w.ErrorRate = errorRatePercent(deltaErr, deltaReq)
	w.P95 = proxy.Quantile(buckets, 0.95)
	w.P99 = proxy.Quantile(buckets, 0.99)
	s.trafficPrev, s.trafficAt = stats, now
	return w
}

func errorRatePercent(errors, requests int64) float64 {
	if requests <= 0 {
		return 0
	}
	return 100 * float64(errors) / float64(requests)
}
