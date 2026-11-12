package deployanalysis

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
)

// MetricSample is one monitor-tick observation. A sample may carry only
// resource data or only traffic data; the boolean flags distinguish "not
// collected" from a real zero.
type MetricSample struct {
	At time.Time `json:"at"`

	HasCPU      bool    `json:"has_cpu,omitempty"`
	CPUPercent  float64 `json:"cpu_percent,omitempty"`
	HasMemory   bool    `json:"has_memory,omitempty"`
	MemoryBytes uint64  `json:"memory_bytes,omitempty"`

	HasTraffic bool    `json:"has_traffic,omitempty"`
	Requests   int64   `json:"requests,omitempty"`
	Errors     int64   `json:"errors,omitempty"`
	P95MS      float64 `json:"p95_ms,omitempty"`
}

const (
	historyFileName = "deployment_metrics.jsonl"
	historyMaxAge   = 6 * time.Hour
	historyMaxBytes = 8 << 20
	historyKeep     = 2000
)

var historyMu sync.Mutex

func historyPath(appName string) (string, error) {
	dir, err := deploy.AppDataDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, historyFileName), nil
}

// RecordAppMetric appends one CPU/memory observation for appName. Failures are
// intentionally silent: metric history is best-effort telemetry and must never
// disturb the monitoring tick.
func RecordAppMetric(appName string, at time.Time, cpuPercent float64, memoryBytes uint64) {
	if appName == "" {
		return
	}
	appendSample(appName, MetricSample{
		At: at, HasCPU: true, CPUPercent: cpuPercent,
		HasMemory: true, MemoryBytes: memoryBytes,
	})
}

// RecordTraffic appends one cumulative proxy-counter observation for appName.
func RecordTraffic(appName string, at time.Time, requests, errors int64, p95 time.Duration) {
	if appName == "" {
		return
	}
	appendSample(appName, MetricSample{
		At: at, HasTraffic: true, Requests: requests, Errors: errors,
		P95MS: float64(p95.Milliseconds()),
	})
}

func appendSample(appName string, sample MetricSample) {
	path, err := historyPath(appName)
	if err != nil {
		return
	}
	historyMu.Lock()
	defer historyMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if info, statErr := os.Stat(path); statErr == nil && info.Size() > historyMaxBytes {
		pruneHistory(path)
	}

	line, err := json.Marshal(sample)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

// pruneHistory keeps the newest historyKeep lines, preserving the tail after a
// byte cap. Best effort: on any error the original file is left untouched.
func pruneHistory(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) <= historyKeep {
		return
	}
	lines = lines[len(lines)-historyKeep:]
	body := strings.Join(lines, "\n") + "\n"
	tmp, err := os.CreateTemp(filepath.Dir(path), ".deployment-metrics-*.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return
	}
	_ = os.Rename(name, path)
}

// ReadSamples returns samples in [start, end], oldest first. Malformed lines
// are skipped rather than failing the whole read, which keeps a partially
// written tail harmless.
func ReadSamples(appName string, start, end time.Time) ([]MetricSample, error) {
	path, err := historyPath(appName)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	cutoff := time.Now().Add(-historyMaxAge)
	samples := make([]MetricSample, 0, 128)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var s MetricSample
		if json.Unmarshal(scanner.Bytes(), &s) != nil || s.At.IsZero() {
			continue
		}
		if s.At.Before(cutoff) || s.At.Before(start) || s.At.After(end) {
			continue
		}
		samples = append(samples, s)
	}
	if err := scanner.Err(); err != nil {
		return samples, err
	}
	return samples, nil
}
