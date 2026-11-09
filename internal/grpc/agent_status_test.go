package grpc

import (
	"slices"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

func TestMonitorStreamAdvertisesWithoutWatchedApps(t *testing.T) {
	setupTestSession(t)
	original := app.Manager
	app.Manager = &watchingGateStub{}
	defer func() { app.Manager = original }()
	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()
	done := make(chan error, 1)
	go func() { done <- c.runMonitorStream() }()
	defer func() {
		c.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("stream did not stop")
		}
	}()
	ev := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool { return ev.GetAgentStatus() != nil }, 3*time.Second)
	if ev == nil {
		t.Fatal("agent status missing without watched apps")
	}
	s := ev.GetAgentStatus()
	if s.GetConnectionId() == "" || s.GetSecurityProtocolVersion() != 2 || !slices.Contains(s.GetCapabilities(), CapabilitySecurityScan) || s.GetHeartbeatIntervalMs() != 10000 {
		t.Fatalf("invalid agent status: %v", s)
	}
	if next := newAgentStatus(); next.ConnectionId == s.ConnectionId {
		t.Fatal("reconnect must have a new connection identity")
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	for _, ev := range backend.received {
		if ev.GetServerInfo() != nil || ev.GetSecurityScan() != nil {
			t.Fatal("status disclosed watched telemetry")
		}
	}
}
