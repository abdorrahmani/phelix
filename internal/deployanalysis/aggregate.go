package deployanalysis

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/health"
)

// WindowAggregate summarizes one observation window. Every field except the
// sample counts is only meaningful when the matching Has flag is true, so a
// missing metric is never mistaken for a measured zero.
type WindowAggregate struct {
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`

	ResourceSamples int `json:"resource_samples"`
	TrafficSamples  int `json:"traffic_samples"`

	HasCPU     bool    `json:"has_cpu,omitempty"`
	CPUAvg     float64 `json:"cpu_avg,omitempty"`
	CPUPeak    float64 `json:"cpu_peak,omitempty"`
	HasMemory  bool    `json:"has_memory,omitempty"`
	MemoryAvg  float64 `json:"memory_avg,omitempty"`
	MemoryPeak float64 `json:"memory_peak,omitempty"`

	HasTraffic     bool    `json:"has_traffic,omitempty"`
	Requests       int64   `json:"requests,omitempty"`
	Errors         int64   `json:"errors,omitempty"`
	ErrorRate      float64 `json:"error_rate,omitempty"`
	P95MS          float64 `json:"p95_ms,omitempty"`
	Throughput     float64 `json:"throughput,omitempty"`
	TrafficSeconds float64 `json:"traffic_seconds,omitempty"`

	Restarts       int `json:"restarts"`
	HealthFailures int `json:"health_failures"`
	HealthChecks   int `json:"health_checks"`
}

// AggregateWindow reduces the recorded metric history and the health daemon's
// history into one window summary. Windows with no data still return a value
// with Has flags false; callers decide whether that is insufficient or absent.
func AggregateWindow(appName string, start, end time.Time) (WindowAggregate, error) {
	agg := WindowAggregate{StartedAt: start, EndedAt: end}

	samples, err := ReadSamples(appName, start, end)
	if err != nil {
		return agg, err
	}

	var cpuSum, memSum float64
	var cpuCount, memCount int
	var trafficSamples int
	var reqSum, errSum int64
	var firstAt, lastAt time.Time
	var p95Sum float64
	var prev *MetricSample

	for i := range samples {
		s := samples[i]
		if s.HasCPU {
			cpuSum += s.CPUPercent
			cpuCount++
			if !agg.HasCPU || s.CPUPercent > agg.CPUPeak {
				agg.CPUPeak = s.CPUPercent
			}
			agg.HasCPU = true
		}
		if s.HasMemory {
			memSum += float64(s.MemoryBytes)
			memCount++
			if !agg.HasMemory || float64(s.MemoryBytes) > agg.MemoryPeak {
				agg.MemoryPeak = float64(s.MemoryBytes)
			}
			agg.HasMemory = true
		}
		if !s.HasTraffic {
			continue
		}
		trafficSamples++
		p95Sum += s.P95MS
		if prev != nil && prev.HasTraffic && s.At.After(prev.At) {
			delta := s.Requests - prev.Requests
			deltaErr := s.Errors - prev.Errors
			if delta >= 0 && deltaErr >= 0 {
				reqSum += delta
				errSum += deltaErr
			}
			if firstAt.IsZero() {
				firstAt = prev.At
			}
			lastAt = s.At
		}
		prev = &samples[i]
	}

	agg.ResourceSamples = max(cpuCount, memCount)
	if cpuCount > 0 {
		agg.CPUAvg = cpuSum / float64(cpuCount)
	}
	if memCount > 0 {
		agg.MemoryAvg = memSum / float64(memCount)
	}
	agg.TrafficSamples = trafficSamples
	if trafficSamples > 0 {
		agg.HasTraffic = true
		agg.P95MS = p95Sum / float64(trafficSamples)
	}
	if reqSum > 0 && !firstAt.IsZero() && lastAt.After(firstAt) {
		agg.Requests = reqSum
		agg.Errors = errSum
		agg.ErrorRate = 100 * float64(errSum) / float64(reqSum)
		agg.TrafficSeconds = lastAt.Sub(firstAt).Seconds()
		agg.Throughput = float64(reqSum) / agg.TrafficSeconds
	} else if trafficSamples > 0 {
		last := samples[len(samples)-1]
		if last.HasTraffic && last.Requests > 0 {
			agg.Requests = last.Requests
			agg.Errors = last.Errors
			agg.ErrorRate = 100 * float64(last.Errors) / float64(last.Requests)
		}
	}

	restarts, checks, failures := readHealthWindow(appName, start, end)
	agg.Restarts = restarts
	agg.HealthChecks = checks
	agg.HealthFailures = failures
	return agg, nil
}

// readHealthWindow counts auto-restarts, health checks and failed checks that
// the health daemon recorded inside the window. Files absent or unreadable
// yield zeros; the callers treat zero checks as "not measured", not "healthy".
func readHealthWindow(appName string, start, end time.Time) (restarts, checks, failures int) {
	dir, err := deploy.AppDataDir(appName)
	if err != nil {
		return 0, 0, 0
	}
	healthDir := filepath.Join(dir, "health")

	if data, err := os.ReadFile(filepath.Join(healthDir, "restarts.json")); err == nil {
		var records []health.AutoRestartRecord
		if json.Unmarshal(data, &records) == nil {
			for _, r := range records {
				if inWindow(r.RestartedAt, start, end) {
					restarts++
				}
			}
		}
	}

	historyDir := filepath.Join(healthDir, "history")
	entries, err := os.ReadDir(historyDir)
	if err != nil {
		return restarts, 0, 0
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(historyDir, entry.Name()))
		if err != nil {
			continue
		}
		var history health.HealthCheckHistory
		if json.Unmarshal(data, &history) != nil {
			continue
		}
		for _, result := range history.Results {
			if !inWindow(result.CheckedAt, start, end) {
				continue
			}
			checks++
			if result.Status != "UP" {
				failures++
			}
		}
	}
	return restarts, checks, failures
}

func inWindow(at, start, end time.Time) bool {
	return !at.Before(start) && !at.After(end)
}

// sortedSamples returns samples ordered by time; the recorder appends in order
// but concurrent writers may interleave, so aggregation sorts defensively.
func sortedSamples(samples []MetricSample) []MetricSample {
	out := append([]MetricSample(nil), samples...)
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// roundTo keeps report values readable without changing their meaning.
func roundTo(v float64, places int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	scale := math.Pow(10, float64(places))
	return math.Round(v*scale) / scale
}
