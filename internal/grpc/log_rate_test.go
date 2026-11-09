package grpc

import (
	"testing"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

// TestLegacyLogEventsAreBounded verifies that one collection pass cannot send
// an unbounded burst on the legacy per-line log channel.
func TestLegacyLogEventsAreBounded(t *testing.T) {
	entries := make([]*pb.MonitorLogEntry, maxLegacyLogEventsPerTick+25)
	if got := len(legacyLogEvents(entries)); got != maxLegacyLogEventsPerTick {
		t.Fatalf("legacy events = %d, want %d", got, maxLegacyLogEventsPerTick)
	}
	small := entries[:3]
	if got := len(legacyLogEvents(small)); got != len(small) {
		t.Fatalf("small burst = %d, want %d", got, len(small))
	}
	if got := len(legacyLogEvents(nil)); got != 0 {
		t.Fatalf("nil burst = %d, want 0", got)
	}
}
