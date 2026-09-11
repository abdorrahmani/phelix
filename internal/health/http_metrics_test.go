package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
)

const caddyMetricsFixture = `# HELP caddy_http_requests_total Total HTTP requests.
# TYPE caddy_http_requests_total counter
caddy_http_requests_total{code="200",handler="",host="Example.COM",server="s1"} 10
caddy_http_requests_total{code="200",handler="",host="other.example",server="s1"} 100
# HELP caddy_http_request_duration_seconds HTTP request duration.
# TYPE caddy_http_request_duration_seconds histogram
caddy_http_request_duration_seconds_bucket{code="200",handler="",host="example.com",le="0.005",server="s1"} 1
caddy_http_request_duration_seconds_bucket{code="200",handler="",host="example.com",le="0.01",server="s1"} 3
caddy_http_request_duration_seconds_bucket{code="200",handler="",host="example.com",le="+Inf",server="s1"} 3
caddy_http_request_duration_seconds_bucket{code="400",handler="",host="example.com",le="0.1",server="s1"} 1
caddy_http_request_duration_seconds_bucket{code="400",handler="",host="example.com",le="+Inf",server="s1"} 2
caddy_http_request_duration_seconds_bucket{code="500",handler="",host="example.com",le="0.25",server="s1"} 2
caddy_http_request_duration_seconds_bucket{code="500",handler="",host="example.com",le="0.5",server="s1"} 6
caddy_http_request_duration_seconds_bucket{code="500",handler="",host="example.com",le="+Inf",server="s1"} 6
`

const caddyMetricsSecondFixture = `# HELP caddy_http_requests_total Total HTTP requests.
# TYPE caddy_http_requests_total counter
caddy_http_requests_total{code="200",handler="",host="example.com",server="s1"} 16
# HELP caddy_http_request_duration_seconds HTTP request duration.
# TYPE caddy_http_request_duration_seconds histogram
caddy_http_request_duration_seconds_bucket{code="200",handler="",host="example.com",le="0.005",server="s1"} 4
caddy_http_request_duration_seconds_bucket{code="200",handler="",host="example.com",le="+Inf",server="s1"} 6
caddy_http_request_duration_seconds_bucket{code="400",handler="",host="example.com",le="0.25",server="s1"} 3
caddy_http_request_duration_seconds_bucket{code="400",handler="",host="example.com",le="+Inf",server="s1"} 4
caddy_http_request_duration_seconds_bucket{code="500",handler="",host="example.com",le="+Inf",server="s1"} 6
`

func TestCaddyMetricsSourceScrape(t *testing.T) {
	var requests, scrapeTime int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			t.Errorf("/metrics requested, got %q", r.URL.Path)
		}
		if atomic.AddInt32(&requests, 1) == 1 {
			_, _ = w.Write([]byte(caddyMetricsFixture))
			return
		}
		_, _ = w.Write([]byte(caddyMetricsSecondFixture))
	}))
	defer server.Close()

	source := NewCaddyMetricsSource()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	source.now = func() time.Time {
		index := int(atomic.AddInt32(&scrapeTime, 1))
		return base.Add(time.Duration(index) * CaddyMetricsInterval)
	}
	cfg := HTTPMetricsConfig{Domain: "Example.com", CaddyAdmin: server.URL}

	first, err := source.Scrape(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first scrape: %v", err)
	}
	if first.HasRequestDelta {
		t.Fatal("first successful scrape should establish baseline only")
	}
	if first.LatencyP50 != 175*time.Millisecond ||
		first.LatencyP95 != 468750*time.Microsecond ||
		first.LatencyP99 != 493750*time.Microsecond {
		t.Fatalf("unexpected estimates: p50=%s p95=%s p99=%s", first.LatencyP50, first.LatencyP95, first.LatencyP99)
	}

	second, err := source.Scrape(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second scrape: %v", err)
	}
	if !second.HasRequestDelta {
		t.Fatal("second successful scrape should contain delta")
	}
	if second.RequestsPerSecond != 3 || second.ClientErrorsPerSec != 1 || second.ServerErrorsPerSec != .5 {
		t.Fatalf("unexpected rates: requests=%f 4xx=%f 5xx=%f", second.RequestsPerSecond, second.ClientErrorsPerSec, second.ServerErrorsPerSec)
	}
}

func TestHTTPMetricsConfigValidateAndURL(t *testing.T) {
	cfg := HTTPMetricsConfig{Domain: "example.com"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default admin URL: %v", err)
	}
	url, err := caddyMetricsURL(cfg.CaddyAdmin)
	if err != nil {
		t.Fatalf("metrics URL: %v", err)
	}
	if url != "http://localhost:2019/metrics" {
		t.Fatalf("unexpected metrics URL %q", url)
	}
	if err := (HTTPMetricsConfig{Domain: "example.com", CaddyAdmin: "ftp://bad"}).Validate(); err == nil {
		t.Fatal("expected invalid admin URL error")
	}
}

func TestParseCaddySample(t *testing.T) {
	families, err := (&expfmt.TextParser{}).TextToMetricFamilies(strings.NewReader(caddyMetricsFixture))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	sample := parseCaddySample(time.Unix(0, 0), "example.com", families)
	for name, family := range families {
		t.Logf("family=%s type=%s metrics=%d", name, family.GetType(), len(family.GetMetric()))
	}
	if sample.histogramSize != 11 || sample.requests != 10 {
		t.Fatalf("unexpected totals: requests=%f histogram=%d", sample.requests, sample.histogramSize)
	}
	if len(sample.buckets) < 7 || sample.buckets[6] != 11 {
		t.Fatalf("unexpected aggregate buckets: %#v", sample.buckets)
	}
}
