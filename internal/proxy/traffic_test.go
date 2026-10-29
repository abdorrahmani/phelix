package proxy

import (
	"testing"
	"time"
)

func TestSummarize(t *testing.T) {
	fast := make([]uint64, len(LatencyBucketBounds))
	for i := range fast {
		fast[i] = 100
	}
	slow := make([]uint64, len(LatencyBucketBounds))
	for i := range slow {
		if i < len(slow)-2 {
			slow[i] = 50
		} else {
			slow[i] = 100
		}
	}

	got := Summarize([]BackendStat{
		{Requests: 100, Errors: 5, Buckets: fast},
		{Requests: 100, Errors: 15, Buckets: slow},
	})
	if got.Requests != 200 || got.Errors != 20 {
		t.Fatalf("requests/errors = %d/%d, want 200/20", got.Requests, got.Errors)
	}
	if rate := got.ErrorRate(); rate != 10 {
		t.Errorf("error rate = %v%%, want 10%%", rate)
	}
	if got.P95 <= 2500*time.Millisecond || got.P95 > 5*time.Second {
		t.Errorf("p95 = %v, want within (2.5s, 5s]", got.P95)
	}

	if empty := Summarize(nil); empty.Requests != 0 || empty.P95 != 0 || empty.ErrorRate() != 0 {
		t.Errorf("empty summary = %+v, want zero", empty)
	}
}

func TestBackendStatShareAndErrorRate(t *testing.T) {
	s := BackendStat{Requests: 25, Errors: 1}
	if got := s.Share(100); got != 25 {
		t.Errorf("share = %v, want 25", got)
	}
	if got := s.ErrorRate(); got != 4 {
		t.Errorf("error rate = %v, want 4", got)
	}
	if got := (BackendStat{}).Share(0); got != 0 {
		t.Errorf("share of zero total = %v, want 0", got)
	}
	if got := (BackendStat{}).ErrorRate(); got != 0 {
		t.Errorf("error rate of no requests = %v, want 0", got)
	}
	if got := (BackendStat{}).P95(); got != 0 {
		t.Errorf("p95 of no histogram = %v, want 0", got)
	}
}
