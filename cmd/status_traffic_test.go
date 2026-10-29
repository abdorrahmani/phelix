package cmd

import (
	"testing"

	"github.com/abdorrahmani/phelix/internal/proxy"
)

func TestBuildStatusTraffic(t *testing.T) {
	if got := buildStatusTraffic(nil); got != nil {
		t.Fatalf("expected nil traffic for an unproxied app, got %+v", got)
	}

	got := buildStatusTraffic([]proxy.BackendStat{
		{Label: "replica-0", Host: "127.0.0.1:9001", Requests: 30, Errors: 3},
		{Label: "replica-1", Host: "127.0.0.1:9002", Requests: 10},
	})
	if got.Requests != 40 || got.Errors != 3 {
		t.Fatalf("requests/errors = %d/%d, want 40/3", got.Requests, got.Errors)
	}
	if got.ErrorRate != 7.5 {
		t.Errorf("error rate = %v, want 7.5", got.ErrorRate)
	}
	if len(got.Backends) != 2 {
		t.Fatalf("backends = %d, want 2", len(got.Backends))
	}
	if got.Backends[0].Share != 75 || got.Backends[1].Share != 25 {
		t.Errorf("shares = %v/%v, want 75/25", got.Backends[0].Share, got.Backends[1].Share)
	}
	if got.Backends[0].ErrorRate != 10 || got.Backends[1].ErrorRate != 0 {
		t.Errorf("per-backend rates = %v/%v, want 10/0",
			got.Backends[0].ErrorRate, got.Backends[1].ErrorRate)
	}
}
