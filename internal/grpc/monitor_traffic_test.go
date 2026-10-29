package grpc

import (
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/monitor"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

func TestToProtoAppTraffic(t *testing.T) {
	buckets := make([]uint64, len(proxy.LatencyBucketBounds))
	for i := range buckets {
		if i < len(buckets)-2 {
			buckets[i] = 50
		} else {
			buckets[i] = 100
		}
	}

	got := toProtoAppTraffic(monitor.AppTraffic{
		AppID: "app-1", AppName: "myapp", ServerID: "srv-1",
		PublicPort: 8080, InFlight: 2, Requests: 100, Errors: 5,
		P95: proxy.Quantile(buckets, 0.95),
		Backends: []monitor.BackendTraffic{{
			Label: "replica-0", Host: "127.0.0.1:9001",
			Requests: 75, Errors: 3, P95: 40 * time.Millisecond, Share: 75,
		}},
	})

	if got.GetAppId() != "app-1" || got.GetAppName() != "myapp" || got.GetServerId() != "srv-1" {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if got.GetInFlight() != 2 || got.GetRequests() != 100 || got.GetErrors() != 5 {
		t.Fatalf("counters mismatch: %+v", got)
	}
	if got.GetErrorRatePercent() != 5 {
		t.Errorf("error rate = %v, want 5", got.GetErrorRatePercent())
	}
	if got.GetP95LatencyMs() <= 2500 || got.GetP95LatencyMs() > 5000 {
		t.Errorf("p95 = %v ms, want within (2500, 5000]", got.GetP95LatencyMs())
	}
	if len(got.GetBackends()) != 1 {
		t.Fatalf("backends = %d, want 1", len(got.GetBackends()))
	}
	b := got.GetBackends()[0]
	if b.GetLabel() != "replica-0" || b.GetSharePercent() != 75 || b.GetP95LatencyMs() != 40 {
		t.Fatalf("backend mismatch: %+v", b)
	}
	if b.GetErrorRatePercent() != 4 {
		t.Errorf("backend error rate = %v, want 4", b.GetErrorRatePercent())
	}
}

func TestToProtoAppTraffic_NoRequests(t *testing.T) {
	got := toProtoAppTraffic(monitor.AppTraffic{AppName: "idle"})
	if got.GetErrorRatePercent() != 0 || got.GetP95LatencyMs() != 0 {
		t.Fatalf("idle app reports rate %v / p95 %v, want 0/0", got.GetErrorRatePercent(), got.GetP95LatencyMs())
	}
	if got.GetBackends() == nil {
		t.Error("backends must be an empty list, not nil")
	}
}
