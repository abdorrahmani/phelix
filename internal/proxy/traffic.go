package proxy

import "time"

// Quantile estimates a latency quantile from a cumulative histogram such as
// BackendStat.Buckets, interpolating inside the bucket that holds the rank.
// It uses the same estimation the health package applies to Caddy histograms.
func Quantile(cumulative []uint64, q float64) time.Duration {
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
	for i, bound := range LatencyBucketBounds {
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
	return LatencyBucketBounds[len(LatencyBucketBounds)-1]
}

// P95 returns this backend's p95 request latency over its cumulative
// histogram.
func (s BackendStat) P95() time.Duration {
	return Quantile(s.Buckets, 0.95)
}

// ErrorRate returns the share of this backend's requests that failed, in
// percent.
func (s BackendStat) ErrorRate() float64 {
	return errorRatePercent(s.Errors, s.Requests)
}

// Share returns this backend's percent of the traffic that served total
// requests.
func (s BackendStat) Share(total int64) float64 {
	if total <= 0 {
		return 0
	}
	return 100 * float64(s.Requests) / float64(total)
}

// TrafficSummary is the app-level rollup over all of its backends.
type TrafficSummary struct {
	Requests int64
	Errors   int64
	P95      time.Duration
}

// ErrorRate returns the share of the app's requests that failed, in percent.
func (t TrafficSummary) ErrorRate() float64 {
	return errorRatePercent(t.Errors, t.Requests)
}

func errorRatePercent(errors, requests int64) float64 {
	if requests <= 0 {
		return 0
	}
	return 100 * float64(errors) / float64(requests)
}

// Summarize merges every backend's counters into one app-level view. Bucket
// counts are cumulative per backend, so summing them per bucket yields the
// cumulative histogram of every request the app served.
func Summarize(stats []BackendStat) TrafficSummary {
	var sum TrafficSummary
	if len(stats) == 0 {
		return sum
	}
	merged := make([]uint64, len(LatencyBucketBounds))
	for _, s := range stats {
		sum.Requests += s.Requests
		sum.Errors += s.Errors
		for i := range merged {
			if i < len(s.Buckets) {
				merged[i] += s.Buckets[i]
			}
		}
	}
	sum.P95 = Quantile(merged, 0.95)
	return sum
}
