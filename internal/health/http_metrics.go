package health

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	ioprom "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

// HTTPMetricsSource reads HTTP-level metrics from a reverse proxy or another
// provider. Implementations must return interval-normalized snapshots without
// exposing provider-specific types to callers.
type HTTPMetricsSource interface {
	Scrape(ctx context.Context, cfg HTTPMetricsConfig) (*HTTPMetricSnapshot, error)
}

// HTTPMetricsConfig opts an application into HTTP metrics collection.
type HTTPMetricsConfig struct {
	Domain     string `json:"domain,omitempty"`
	CaddyAdmin string `json:"caddy_admin,omitempty"`
}

// Configured reports whether enough configuration was supplied for scraping.
func (cfg HTTPMetricsConfig) Configured() bool {
	return cfg.Domain != ""
}

// Validate rejects provider configuration that cannot identify a host or
// admin endpoint before it is persisted.
func (cfg HTTPMetricsConfig) Validate() error {
	if cfg.Domain == "" {
		return fmt.Errorf("HTTP metrics domain is required")
	}
	parsed, err := url.Parse(cfg.caddyAdmin())
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("invalid Caddy admin URL %q", cfg.CaddyAdmin)
	}
	return nil
}

// HTTPMetricSnapshot contains one refresh interval of HTTP activity plus
// latency estimates taken at the end of that interval.
type HTTPMetricSnapshot struct {
	ObservedAt         time.Time `json:"-"`
	HasRequestDelta    bool      `json:"-"`
	RequestsPerSecond  float64   `json:"-"`
	ClientErrorsPerSec float64   `json:"-"`
	ServerErrorsPerSec float64   `json:"-"`
	LatencyP50         time.Duration
	LatencyP95         time.Duration
	LatencyP99         time.Duration
}

const caddyMetricsTimeout = 2 * time.Second

const CaddyMetricsInterval = 2 * time.Second

var caddyLatencyBuckets = [...]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// CaddyMetricsSource is the built-in HTTPMetricsSource implementation. It is
// read-only: it scrapes the user-configured admin endpoint and never changes
// Caddy configuration.
type CaddyMetricsSource struct {
	client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	samples map[string]caddySample
}

type caddySample struct {
	observedAt    time.Time
	requests      float64
	clientErrors  float64
	serverErrors  float64
	histogramSize uint64
	buckets       []float64
}

// NewCaddyMetricsSource creates a source backed by Caddy's Prometheus metrics.
func NewCaddyMetricsSource() *CaddyMetricsSource {
	return &CaddyMetricsSource{
		client:  &http.Client{Timeout: caddyMetricsTimeout},
		now:     time.Now,
		samples: make(map[string]caddySample),
	}
}

// Scrape fetches and converts Caddy metrics for one application. The first
// successful scrape establishes counter baselines and therefore has no rate
// values; subsequent scrapes contain deltas over the elapsed interval.
func (s *CaddyMetricsSource) Scrape(ctx context.Context, cfg HTTPMetricsConfig) (*HTTPMetricSnapshot, error) {
	if !cfg.Configured() {
		return nil, fmt.Errorf("HTTP metrics domain is required")
	}

	metricsURL, err := caddyMetricsURL(cfg.CaddyAdmin)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metricsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create Caddy metrics request: %w", err)
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request Caddy metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("Caddy metrics returned %s", resp.Status)
	}

	families, err := (&expfmt.TextParser{}).TextToMetricFamilies(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("parse Caddy metrics: %w", err)
	}

	sample := parseCaddySample(s.now(), cfg.Domain, families)
	key := cfg.Domain + "\x00" + cfg.caddyAdmin()
	var snapshot HTTPMetricSnapshot

	s.mu.Lock()
	previous, existed := s.samples[key]
	s.samples[key] = sample
	s.mu.Unlock()

	snapshot.ObservedAt = sample.observedAt
	snapshot.LatencyP50, snapshot.LatencyP95, snapshot.LatencyP99 = estimateHistogramQuantiles(sample)
	if existed {
		elapsed := sample.observedAt.Sub(previous.observedAt).Seconds()
		if elapsed <= 0 {
			return &snapshot, nil
		}
		snapshot.HasRequestDelta = true
		snapshot.RequestsPerSecond = counterDelta(previous.requests, sample.requests) / elapsed
		snapshot.ClientErrorsPerSec = counterDelta(previous.clientErrors, sample.clientErrors) / elapsed
		snapshot.ServerErrorsPerSec = counterDelta(previous.serverErrors, sample.serverErrors) / elapsed
	}
	return &snapshot, nil
}

func (cfg HTTPMetricsConfig) caddyAdmin() string {
	if cfg.CaddyAdmin == "" {
		return "http://localhost:2019"
	}
	return cfg.CaddyAdmin
}

func caddyMetricsURL(adminBase string) (string, error) {
	base := adminBase
	if base == "" {
		base = "http://localhost:2019"
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("invalid Caddy admin URL %q", adminBase)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/metrics"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func parseCaddySample(observedAt time.Time, configuredDomain string, families map[string]*ioprom.MetricFamily) caddySample {
	domain := strings.ToLower(configuredDomain)
	sample := caddySample{
		observedAt: observedAt,
		buckets:    make([]float64, len(caddyLatencyBuckets)),
	}

	if family, ok := families["caddy_http_requests_total"]; ok {
		for _, metric := range family.GetMetric() {
			if labelValue(metric.GetLabel(), "host") != domain {
				continue
			}
			sample.requests += metric.GetCounter().GetValue()
		}
	}

	family, ok := families["caddy_http_request_duration_seconds"]
	if !ok {
		return sample
	}

	bucketTotals := make(map[float64]float64)
	for _, metric := range family.GetMetric() {
		if labelValue(metric.GetLabel(), "host") != domain {
			continue
		}

		histogram := metric.GetHistogram()
		count := histogram.GetSampleCount()
		sample.histogramSize += count

		switch statusCodeClass(labelValue(metric.GetLabel(), "code")) {
		case 4:
			sample.clientErrors += float64(count)
		case 5:
			sample.serverErrors += float64(count)
		}

		seriesBuckets := make(map[float64]float64)
		for _, bucket := range histogram.GetBucket() {
			upperBound := bucket.GetUpperBound()
			if _, known := knownCaddyBucket(upperBound); known {
				seriesBuckets[upperBound] += float64(bucket.GetCumulativeCount())
			}
		}

		seriesCumulative := 0.0
		for _, boundary := range caddyLatencyBuckets {
			if value, ok := seriesBuckets[boundary]; ok {
				seriesCumulative = value
			}
			bucketTotals[boundary] += seriesCumulative
		}
	}

	for index, boundary := range caddyLatencyBuckets {
		sample.buckets[index] = bucketTotals[boundary]
	}
	if sample.requests == 0 {
		sample.requests = float64(sample.histogramSize)
	}
	return sample
}

func knownCaddyBucket(boundary float64) (int, bool) {
	for index, candidate := range caddyLatencyBuckets {
		if candidate == boundary {
			return index, true
		}
	}
	return 0, false
}

func labelValue(labels []*ioprom.LabelPair, wanted string) string {
	for _, label := range labels {
		if label.GetName() == wanted {
			return strings.ToLower(label.GetValue())
		}
	}
	return ""
}

func statusCodeClass(code string) int {
	value, err := strconv.Atoi(code)
	if err != nil || value < 100 {
		return 0
	}
	return value / 100
}

func estimateHistogramQuantiles(sample caddySample) (time.Duration, time.Duration, time.Duration) {
	total := float64(sample.histogramSize)
	if total == 0 {
		return 0, 0, 0
	}
	return estimateQuantile(sample, total, .50),
		estimateQuantile(sample, total, .95),
		estimateQuantile(sample, total, .99)
}

func estimateQuantile(sample caddySample, total, quantile float64) time.Duration {
	rank := quantile * total
	previousBoundary := 0.0
	previousCount := 0.0
	for index, boundary := range caddyLatencyBuckets {
		count := sample.buckets[index]
		if count >= rank {
			if count == previousCount {
				break
			}
			seconds := previousBoundary + (rank-previousCount)/(count-previousCount)*(boundary-previousBoundary)
			return time.Duration(seconds * float64(time.Second))
		}
		previousBoundary = boundary
		previousCount = count
	}
	return time.Duration(caddyLatencyBuckets[len(caddyLatencyBuckets)-1] * float64(time.Second))
}

func counterDelta(previous, current float64) float64 {
	if math.IsNaN(previous) || math.IsInf(previous, 0) || math.IsNaN(current) || math.IsInf(current, 0) || current < previous {
		return math.Max(current, 0)
	}
	return current - previous
}
