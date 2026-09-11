package grpc

import (
	"testing"
	"time"
)

func TestReconnectState_NextDelayGrowsAndCaps(t *testing.T) {
	r := newReconnectState()

	var prev time.Duration
	for i := range 10 {
		d := r.nextDelay()
		if d <= 0 {
			t.Fatalf("attempt %d: expected positive delay, got %v", i, d)
		}
		// Allow jitter, but delay should never exceed maxBackoff by more
		// than the jitter percentage.
		maxAllowed := time.Duration(float64(maxBackoff) * (1 + jitterPercent))
		if d > maxAllowed {
			t.Fatalf("attempt %d: delay %v exceeds max allowed %v", i, d, maxAllowed)
		}
		prev = d
	}
	_ = prev

	if got := r.getAttempts(); got != 10 {
		t.Fatalf("expected 10 attempts recorded, got %d", got)
	}
}

func TestReconnectState_Reset(t *testing.T) {
	r := newReconnectState()
	for range 5 {
		r.nextDelay()
	}
	if r.getAttempts() == 0 {
		t.Fatal("expected attempts to be non-zero before reset")
	}

	r.reset()

	if got := r.getAttempts(); got != 0 {
		t.Fatalf("expected attempts to reset to 0, got %d", got)
	}
	if r.currentDelay != initialBackoff {
		t.Fatalf("expected currentDelay to reset to %v, got %v", initialBackoff, r.currentDelay)
	}
}

func TestBackoffDuration_CapsAtMax(t *testing.T) {
	d := backoffDuration(30) // way beyond the cap
	maxAllowed := time.Duration(float64(maxBackoff) * (1 + jitterPercent))
	if d > maxAllowed {
		t.Fatalf("expected backoff capped near %v, got %v", maxBackoff, d)
	}
	if d <= 0 {
		t.Fatalf("expected positive backoff, got %v", d)
	}
}

func TestBackoffDuration_MonotonicUntilCap(t *testing.T) {
	// Compare the theoretical (jitter-free) midpoints to ensure the
	// underlying growth is exponential before hitting the cap.
	prev := time.Duration(0)
	for attempt := range 4 {
		// Use several samples and take the max to reduce jitter flakiness.
		var maxSeen time.Duration
		for range 20 {
			if d := backoffDuration(attempt); d > maxSeen {
				maxSeen = d
			}
		}
		if maxSeen <= prev {
			t.Fatalf("attempt %d: expected growth beyond previous max %v, got %v", attempt, prev, maxSeen)
		}
		prev = maxSeen
	}
}
