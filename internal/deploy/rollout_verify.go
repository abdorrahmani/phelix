package deploy

import (
	"context"
	"errors"
	"fmt"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// Canary step verification: the observation each rollout step must pass before
// the next traffic share is applied. Verification is deliberately layered the
// way the deployment tiers are:
//
//  1. Health — the canary (and the stable baseline) must answer the app's
//     deploy-tier health check on every sweep of the window. This part always
//     runs; it is the same primitive blue-green gates its cut-over on.
//  2. Metrics — when the proxy daemon can serve per-backend statistics, the
//     traffic the canary actually served during the window is compared against
//     the stable baseline: error rate (absolute cap and delta over baseline)
//     and p95 latency (factor over baseline). Thresholds come from the rollout
//     plan's VerificationConfig. When metrics are unavailable (older daemon,
//     control error) the rollout degrades to health-only verification instead
//     of failing: never routing to an unhealthy canary is guaranteed by (1).

// ErrStableBaselineUnhealthy marks a verification failure caused by the STABLE
// side dying mid-rollout. The rollout still fails, but the engine must not
// stop the canary in that case: it may be the only backend still serving.
var ErrStableBaselineUnhealthy = errors.New("stable baseline failed its health probe during rollout")

// StepContext is everything one verification window needs to know about the
// step and the two instances it observes.
type StepContext struct {
	Step           int // zero-based index of the step being verified
	TotalSteps     int
	TrafficPercent int
	Duration       time.Duration

	CanarySlot    string
	CanaryPort    int
	CanaryPID     int
	CanaryVersion int

	StableSlot    string
	StablePort    int
	StablePID     int
	StableVersion int

	Tier    health.Tier
	TierCfg *health.DeployTierConfig
}

// StepVerifier observes the canary during one step's window and returns nil
// when the rollout may proceed to the next step.
type StepVerifier func(ctx context.Context, sc StepContext) error

// verifyStep is the default StepVerifier: health sweeps for the whole window
// plus a baseline comparison of the traffic both sides actually served.
func (ro *Rollout) verifyStep(ctx context.Context, sc StepContext) error {
	log := ro.logger()
	interval := ro.Plan.Verification.withDefaults().Interval

	probe := func() error {
		if sc.CanaryPID > 0 && !pidAlive(sc.CanaryPID) {
			return phelixerr.Newf(phelixerr.CodeCanaryRegression,
				"canary process (pid %d) exited while serving %d%% of traffic", sc.CanaryPID, sc.TrafficPercent)
		}
		if !health.Check(ctx, sc.Tier, sc.TierCfg, hostPort(sc.CanaryPort), sc.CanaryPID, nil) {
			return phelixerr.Newf(phelixerr.CodeCanaryRegression,
				"canary failed its health check at %d%% traffic (tier %s)", sc.TrafficPercent, sc.Tier)
		}
		if !health.Check(ctx, sc.Tier, sc.TierCfg, hostPort(sc.StablePort), sc.StablePID, nil) {
			return phelixerr.Wrapf(phelixerr.CodeHealthCheckFailed, ErrStableBaselineUnhealthy,
				"stable baseline on port %d failed its health check during rollout", sc.StablePort)
		}
		return nil
	}

	if sc.Duration > 0 {
		log.Stepf("monitoring canary for %s (health and metrics every %s)", sc.Duration, interval)
	} else {
		log.Stepf("verifying canary health at %d%% traffic", sc.TrafficPercent)
	}

	// Snapshot the per-backend counters at window start so the end snapshot
	// yields windowed rates; nil means metrics are unavailable this rollout.
	startStats := ro.snapshotStats(ctx)
	if err := probe(); err != nil {
		return err
	}
	if sc.Duration > 0 {
		deadline := time.Now().Add(sc.Duration)
		for time.Now().Before(deadline) {
			wait := interval
			if remaining := time.Until(deadline); remaining < wait {
				wait = remaining
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			if err := probe(); err != nil {
				return err
			}
		}
	}
	endStats := ro.snapshotStats(ctx)
	return ro.evaluateStats(sc, startStats, endStats)
}

// snapshotStats reads one per-backend counter snapshot from the proxy daemon,
// or nil when this rollout cannot use metrics.
func (ro *Rollout) snapshotStats(ctx context.Context) map[string]proxy.BackendStat {
	if ro.statsBroken {
		return nil
	}
	sc, ok := ro.ProxyClient.(StatsClient)
	if !ok {
		ro.markStatsUnavailable("proxy client does not expose per-backend metrics", nil)
		return nil
	}
	stats, err := sc.Stats(ctx, ro.AppName)
	if err != nil {
		ro.markStatsUnavailable("proxy metrics query failed", err)
		return nil
	}
	out := make(map[string]proxy.BackendStat, len(stats))
	for _, s := range stats {
		out[s.Host] = s
	}
	return out
}

func (ro *Rollout) markStatsUnavailable(reason string, err error) {
	if !ro.statsWarned {
		ro.statsWarned = true
		if err != nil {
			ro.logger().Infof("%s: %v; continuing with health-only verification", reason, err)
		} else {
			ro.logger().Infof("%s; continuing with health-only verification", reason)
		}
	}
	ro.statsBroken = true
}

// statDelta is the traffic one backend served between two snapshots.
type statDelta struct {
	requests int64
	errors   int64
	buckets  []uint64 // cumulative within the window; nil when unavailable
}

// evaluateStats compares the traffic the canary served during the window
// against the stable baseline. Both snapshots must be non-nil (metrics were
// available); with no canary traffic there is nothing to compare, which is
// recorded rather than failed — a canary at 5% of an idle app legitimately
// serves zero requests.
func (ro *Rollout) evaluateStats(sc StepContext, start, end map[string]proxy.BackendStat) error {
	if start == nil || end == nil {
		return nil
	}
	log := ro.logger()
	v := ro.Plan.Verification.withDefaults()

	canaryHost, stableHost := hostPort(sc.CanaryPort), hostPort(sc.StablePort)
	cDelta := deltaBetween(start[canaryHost], end[canaryHost])
	sDelta := deltaBetween(start[stableHost], end[stableHost])

	canaryVer, stableVer := versionLabel(sc.CanaryVersion), stableVersionLabel(&Instance{Version: sc.StableVersion})
	if canaryVer == "" {
		canaryVer = "canary"
	}
	log.Infof("%s: %s requests, %.2f%% errors, p95 %s",
		canaryVer, humanCount(cDelta.requests), errorRate(cDelta.errors, cDelta.requests), p95Label(cDelta))
	if sDelta.requests > 0 || sDelta.errors > 0 {
		log.Infof("%s: %s requests, %.2f%% errors, p95 %s",
			stableVer, humanCount(sDelta.requests), errorRate(sDelta.errors, sDelta.requests), p95Label(sDelta))
	}

	if cDelta.requests <= 0 {
		log.Infof("no canary traffic observed during the verification window; metrics comparison skipped")
		return nil
	}

	cRate := 100 * float64(cDelta.errors) / float64(cDelta.requests)
	if cRate > v.MaxErrorRate {
		return phelixerr.Newf(phelixerr.CodeCanaryRegression,
			"canary regression detected: canary error rate %.2f%% exceeds the configured maximum of %.2f%%",
			cRate, v.MaxErrorRate)
	}
	if sDelta.requests > 0 {
		sRate := 100 * float64(sDelta.errors) / float64(sDelta.requests)
		if cRate-sRate > v.MaxErrorDelta {
			return phelixerr.Newf(phelixerr.CodeCanaryRegression,
				"canary regression detected: error rate baseline %.2f%% vs canary %.2f%% (delta %.2f%% exceeds the allowed %.2f%%)",
				sRate, cRate, cRate-sRate, v.MaxErrorDelta)
		}
		cP95, sP95 := quantileFromBuckets(cDelta.buckets, 0.95), quantileFromBuckets(sDelta.buckets, 0.95)
		if sP95 > 0 && cP95 > time.Duration(float64(sP95)*v.MaxP95Factor) {
			return phelixerr.Newf(phelixerr.CodeCanaryRegression,
				"canary regression detected: p95 latency %s vs baseline %s (exceeds the configured %.1fx factor)",
				cP95.Round(time.Millisecond), sP95.Round(time.Millisecond), v.MaxP95Factor)
		}
	}
	return nil
}

// deltaBetween computes the windowed traffic between two cumulative snapshots
// of one backend. Counters clamp at zero so a daemon restart mid-window (which
// resets counters) cannot produce negative rates. A backend absent from the
// start snapshot (zero traffic recorded yet) is a zero baseline, not a missing
// histogram: its window histogram is the end snapshot's own.
func deltaBetween(start, end proxy.BackendStat) statDelta {
	d := statDelta{
		requests: end.Requests - start.Requests,
		errors:   end.Errors - start.Errors,
	}
	if d.requests < 0 {
		d.requests = end.Requests
		d.errors = end.Errors
		return d
	}
	if d.errors < 0 {
		d.errors = 0
	}
	boundsLen := len(proxy.LatencyBucketBounds)
	switch {
	case len(end.Buckets) == boundsLen && len(start.Buckets) == boundsLen:
		d.buckets = make([]uint64, boundsLen)
		for i := range d.buckets {
			delta := int64(end.Buckets[i]) - int64(start.Buckets[i])
			if delta < 0 {
				delta = int64(end.Buckets[i]) // counter reset mid-window
			}
			d.buckets[i] = uint64(delta)
		}
	case len(end.Buckets) == boundsLen && len(start.Buckets) == 0:
		d.buckets = append([]uint64(nil), end.Buckets...)
	}
	return d
}

// quantileFromBuckets estimates a latency quantile from a cumulative bucket
// histogram (buckets[i] counts requests within proxy.LatencyBucketBounds[i]),
// interpolating inside the bucket that contains the rank — the same estimation
// the health package applies to Caddy histograms.
func quantileFromBuckets(cumulative []uint64, q float64) time.Duration {
	if len(cumulative) == 0 {
		return 0
	}
	total := float64(cumulative[len(cumulative)-1])
	if total <= 0 {
		return 0
	}
	rank := q * total
	prevBound := time.Duration(0)
	prevCount := float64(0)
	for i, bound := range proxy.LatencyBucketBounds {
		if i >= len(cumulative) {
			break
		}
		count := float64(cumulative[i])
		if count >= rank {
			if count == prevCount {
				break
			}
			frac := (rank - prevCount) / (count - prevCount)
			return prevBound + time.Duration(frac*float64(bound-prevBound))
		}
		prevBound, prevCount = bound, count
	}
	return proxy.LatencyBucketBounds[len(proxy.LatencyBucketBounds)-1]
}

// --- display helpers ---------------------------------------------------------

func errorRate(errCount, requests int64) float64 {
	if requests <= 0 {
		return 0
	}
	return 100 * float64(errCount) / float64(requests)
}

func p95Label(d statDelta) string {
	p95 := quantileFromBuckets(d.buckets, 0.95)
	if p95 == 0 {
		return "n/a"
	}
	return p95.Round(time.Millisecond).String()
}

// humanCount renders a request count with thousands separators for the
// monitoring summary lines.
func humanCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
