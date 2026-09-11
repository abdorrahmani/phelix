package grpc

import (
	"context"
	"net"
	"testing"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// TestReconnectIfNeeded_NudgesNonReadyConnection verifies that
// reconnectIfNeeded() actively nudges a connection that isn't Ready (e.g.
// stuck in TransientFailure or Idle) instead of only reacting to Shutdown,
// which was the root cause of connections never recovering after a backend
// restart.
func TestReconnectIfNeeded_NudgesNonReadyConnection(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	pb.RegisterPhelixServiceServer(grpcServer, &fakeMonitorBackend{
		newEvent: make(chan struct{}, 8),
		toClient: make(chan *pb.MonitorControl, 1),
	})
	go func() { _ = grpcServer.Serve(lis) }()

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufconn: %v", err)
	}
	defer conn.Close()

	c := &Client{
		done:        make(chan struct{}),
		reconnectCh: make(chan struct{}, 1),
		reconnect:   newReconnectState(),
		conn:        conn,
		connected:   true,
	}
	c.serviceClient = pb.NewPhelixServiceClient(conn)

	// grpc.NewClient starts idle and only connects on an explicit Connect()
	// call or the first RPC; kick it, then wait for the real baseline.
	conn.Connect()
	waitForState(t, conn, connectivity.Ready, 2*time.Second)

	// Shut the server down to force the connection into a non-Ready state.
	grpcServer.Stop()
	_ = lis.Close()

	waitForNotState(t, conn, connectivity.Ready, 2*time.Second)

	// reconnectIfNeeded must not panic and must record that the connection
	// became unready (used to decide when to escalate to a full redial).
	c.reconnectIfNeeded()

	c.mu.RLock()
	unreadySince := c.unreadySince
	c.mu.RUnlock()

	if unreadySince.IsZero() {
		t.Fatal("expected unreadySince to be recorded for a non-Ready connection")
	}
}

// TestReconnectIfNeeded_EscalatesAfterThreshold verifies that once a
// connection has been stuck (non-Ready) longer than staleConnectionThreshold,
// reconnectIfNeeded schedules a full reconnect rather than nudging forever.
func TestReconnectIfNeeded_EscalatesAfterThreshold(t *testing.T) {
	origThreshold := staleConnectionThreshold
	staleConnectionThreshold = 10 * time.Millisecond
	defer func() { staleConnectionThreshold = origThreshold }()

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	pb.RegisterPhelixServiceServer(grpcServer, &fakeMonitorBackend{
		newEvent: make(chan struct{}, 8),
		toClient: make(chan *pb.MonitorControl, 1),
	})
	go func() { _ = grpcServer.Serve(lis) }()

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufconn: %v", err)
	}
	defer conn.Close()

	c := &Client{
		done:        make(chan struct{}),
		reconnectCh: make(chan struct{}, 1),
		reconnect:   newReconnectState(),
		conn:        conn,
		connected:   true,
	}
	c.serviceClient = pb.NewPhelixServiceClient(conn)

	conn.Connect()
	waitForState(t, conn, connectivity.Ready, 2*time.Second)

	grpcServer.Stop()
	_ = lis.Close()
	waitForNotState(t, conn, connectivity.Ready, 2*time.Second)

	// First call records unreadySince.
	c.reconnectIfNeeded()

	// Wait past the (shortened) threshold, then call again — this should
	// escalate to scheduleReconnect(), which posts to reconnectCh.
	time.Sleep(20 * time.Millisecond)
	c.reconnectIfNeeded()

	select {
	case <-c.reconnectCh:
		// Escalation happened as expected.
	case <-time.After(1 * time.Second):
		t.Fatal("expected reconnectIfNeeded to escalate to a full reconnect after the stale threshold")
	}
}

func waitForState(t *testing.T, conn *grpc.ClientConn, want connectivity.State, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		if conn.GetState() == want {
			return
		}
		if !conn.WaitForStateChange(ctx, conn.GetState()) {
			t.Fatalf("timed out waiting for state %v (last state: %v)", want, conn.GetState())
		}
	}
}

func waitForNotState(t *testing.T, conn *grpc.ClientConn, unwanted connectivity.State, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		if conn.GetState() != unwanted {
			return
		}
		if !conn.WaitForStateChange(ctx, conn.GetState()) {
			t.Fatalf("timed out waiting to leave state %v", unwanted)
		}
	}
}
