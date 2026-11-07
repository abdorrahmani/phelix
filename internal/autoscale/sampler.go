// Package autoscale hosts the automatic autoscaling controller: a per-app
// metrics sampler, the pause/history persistence and the evaluation loop
// that turns the deploy package's decision engine into real scaling
// operations. It adds no daemon: the controller runs inside the existing
// monitor process, and every replica mutation goes through the existing
// scaling paths.
package autoscale

import (
	"context"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/process"
)

// ProxyStatsSource is the slice of the proxy control client the sampler
// needs. *proxy.Client satisfies it.
type ProxyStatsSource interface {
	Stats(ctx context.Context, appName string) ([]proxy.BackendStat, error)
	Status(ctx context.Context, appName string) ([]proxy.AppStatus, error)
}

// ReplicaRef identifies one replica to sample: the deploy.json slot key and
// its process ID.
type ReplicaRef struct {
	Key string
	PID int
}

// ReplicaSample is one replica's collected resource usage.
type ReplicaSample struct {
	Key         string
	PID         int
	CPUPercent  float64
	MemoryBytes uint64
	At          time.Time
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

// procReader abstracts per-PID stats collection so tests can feed
// deterministic values.
type procReader interface {
	// busySeconds returns the process's cumulative user+system CPU time.
	busySeconds(pid int) (float64, error)
	// rssBytes returns the process's resident set size.
	rssBytes(pid int) (uint64, error)
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

// Sampler collects per-replica CPU and memory plus windowed proxy traffic
// for one app. It retains the previous CPU-time and traffic-counter state
// needed to turn cumulative counters into interval deltas, so one instance
// must be reused per app across collections.
type Sampler struct {
	app   string
	stats ProxyStatsSource

	// now, reader and hostMemoryBytes are test seams; production leaves them
	// at their defaults.
	now             func() time.Time
	reader          procReader
	hostMemoryBytes func() (uint64, error)

	mu          sync.Mutex
	cpuPrev     map[int]procSample
	trafficPrev []proxy.BackendStat
	trafficAt   time.Time
	everApplied bool
}

// NewSampler builds a sampler for one app. stats may be nil for apps without
// proxy traffic (workers); traffic is then permanently not applicable.
func NewSampler(app string, stats ProxyStatsSource) *Sampler {
	return &Sampler{
		app:             app,
		stats:           stats,
		now:             time.Now,
		reader:          gopsutilReader{},
		hostMemoryBytes: defaultHostMemoryBytes,
	}
}

func defaultHostMemoryBytes() (uint64, error) {
	vm, err := mem.VirtualMemory()
	if err != nil {
		return 0, err
	}
	return vm.Total, nil
}

// Collect takes one observation of the given live replicas and the app's
// proxy counters. Replicas whose process could not be read are skipped and
// flag the corresponding dimension missing at the app level: the engine
// never scales down on data it could not see.
func (s *Sampler) Collect(ctx context.Context, replicas []ReplicaRef) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	snap := Snapshot{App: s.app, At: now}
	snap.Metrics.At = now

	live := make([]ReplicaRef, 0, len(replicas))
	for _, r := range replicas {
		if r.PID > 0 {
			live = append(live, r)
		}
	}

	hostTotal, hostErr := s.hostMemoryBytes()
	prevCPU := s.cpuPrev
	s.cpuPrev = make(map[int]procSample, len(live))

	var (
		maxCPU      float64
		haveCPU     bool
		maxMemPct   float64
		haveMem     bool
		cpuFailed   bool
		memFailed   bool
		replicasOut []ReplicaSample
	)
	for _, r := range live {
		rs := ReplicaSample{Key: r.Key, PID: r.PID, At: now}
		if total, err := s.reader.busySeconds(r.PID); err == nil {
			if prev, ok := prevCPU[r.PID]; ok {
				elapsed := now.Sub(prev.at).Seconds()
				if elapsed > 0 {
					pct := (total - prev.total) / elapsed * 100
					if pct < 0 {
						pct = 0
					}
					rs.CPUPercent = pct
					haveCPU = true
					if pct > maxCPU {
						maxCPU = pct
					}
				} else {
					cpuFailed = true
				}
			} else {
				// First observation of this PID: no interval to diff over, so
				// the CPU value is unavailable this round.
				cpuFailed = true
			}
			s.cpuPrev[r.PID] = procSample{total: total, at: now}
		} else {
			cpuFailed = true
			memFailed = true
			s.cpuPrev[r.PID] = procSample{}
		}
		if rss, err := s.reader.rssBytes(r.PID); err == nil && hostErr == nil && hostTotal > 0 {
			rs.MemoryBytes = rss
			pct := float64(rss) / float64(hostTotal) * 100
			haveMem = true
			if pct > maxMemPct {
				maxMemPct = pct
			}
		} else if err == nil && hostErr != nil {
			memFailed = true
		} else {
			memFailed = true
		}
		replicasOut = append(replicasOut, rs)
	}
	snap.Replicas = replicasOut

	if haveCPU {
		snap.Metrics.CPUPercent = maxCPU
	}
	if haveMem {
		snap.Metrics.MemoryPercent = maxMemPct
	}
	if !haveCPU {
		snap.Metrics.Missing |= deploy.MissingCPU
	} else if cpuFailed {
		// Some replica's CPU was unreadable: shrink-safety wins over the
		// aggregate, so scale-down is blocked this round.
		snap.Metrics.Missing |= deploy.MissingCPU
	}
	if !haveMem || memFailed {
		snap.Metrics.Missing |= deploy.MissingMemory
	}

	snap.Traffic = s.collectTraffic(ctx, now)
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

// collectTraffic diffs the proxy's cumulative backend counters into one
// windowed observation. Negative deltas after a proxy daemon restart are
// clamped to zero, the same way rollout verification protects its windows.
func (s *Sampler) collectTraffic(ctx context.Context, now time.Time) TrafficWindow {
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
