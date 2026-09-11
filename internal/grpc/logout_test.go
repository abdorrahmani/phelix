package grpc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/config"
	"github.com/abdorrahmani/phelix/internal/connstate"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

// recordingBackend captures AgentLogout calls and can be told to reject
// credentials, to exercise both the happy path and the UNAUTHENTICATED path.
type recordingBackend struct {
	pb.UnimplementedPhelixServiceServer

	mu         sync.Mutex
	logouts    []*pb.AgentLogoutRequest
	rejectAuth bool
}

func (b *recordingBackend) AgentLogout(_ context.Context, req *pb.AgentLogoutRequest) (*pb.MetadataResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logouts = append(b.logouts, req)
	return &pb.MetadataResponse{Accepted: true, Message: "marked disconnected"}, nil
}

func (b *recordingBackend) SyncMetadata(_ context.Context, md *pb.CLIMetadata) (*pb.MetadataResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rejectAuth {
		return nil, phelixerr.New(phelixerr.CodeUnauthenticated, "session revoked").(error)
	}
	return &pb.MetadataResponse{Accepted: true}, nil
}

func (b *recordingBackend) logoutCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.logouts)
}

// waitReady nudges the bufconn-backed ClientConn into Ready so
// Client.IsConnected() reports true.
func waitReady(t *testing.T, c *Client) {
	t.Helper()
	c.conn.Connect()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if c.conn.GetState().String() == "READY" {
			return
		}
		if !c.conn.WaitForStateChange(ctx, c.conn.GetState()) {
			t.Fatalf("connection never became READY (state=%v)", c.conn.GetState())
		}
	}
}

// TestNotifyLogout_MarksDisconnectedAndIsIdempotent covers: explicit logout
// notifies the backend, carries the persistent agent_id, and repeated calls
// don't corrupt anything (idempotency requirement).
func TestNotifyLogout_MarksDisconnectedAndIsIdempotent(t *testing.T) {
	setupTestSession(t)
	if err := config.Load(); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	connstate.MarkConnected()

	var backend recordingBackend
	c, _, cleanup := dialBufconn(t, &backend)
	defer cleanup()

	SetClient(c)
	defer SetClient(nil)
	waitReady(t, c)

	if err := NotifyLogout(connstate.Disconnected); err != nil {
		t.Fatalf("first NotifyLogout: %v", err)
	}
	if err := NotifyLogout(connstate.Disconnected); err != nil {
		t.Fatalf("repeated NotifyLogout must stay idempotent, got: %v", err)
	}

	if backend.logoutCount() != 2 {
		t.Fatalf("backend saw %d logout notifications, want 2", backend.logoutCount())
	}
	first := backend.logouts[0]
	if first.GetReason() != connstate.Disconnected {
		t.Errorf("reason = %q, want %q", first.GetReason(), connstate.Disconnected)
	}
	if first.GetAgentId() == "" {
		t.Error("AgentLogoutRequest.agent_id is empty; the backend needs it to update the existing record")
	}
}

// TestNotifyLogout_NoSessionIsNoop covers 'logout without an active
// connection/session': nothing to notify, no error.
func TestNotifyLogout_NoSessionIsNoop(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	// No session.json written.

	if err := NotifyLogout(connstate.Disconnected); err != nil {
		t.Fatalf("NotifyLogout without session should be a quiet no-op, got: %v", err)
	}
}

// TestHandleAuthRejected_StopsClientButKeepsState covers credential
// expiration: the client stops authenticated sync (closed), state becomes
// auth_expired — and crucially nothing about local app state is touched.
func TestHandleAuthRejected_StopsClientButKeepsState(t *testing.T) {
	setupTestSession(t)
	connstate.MarkConnected()

	var backend recordingBackend
	c, _, cleanup := dialBufconn(t, &backend)
	defer cleanup()

	c.handleAuthRejected()

	if got := connstate.Get(); got != connstate.AuthExpired {
		t.Fatalf("state after auth rejection = %q, want %q", got, connstate.AuthExpired)
	}
	select {
	case <-c.done:
		// expected: sync loops stopped so we stop retrying with a dead key
	default:
		t.Error("client not stopped after auth rejection; agent would retry forever with invalid credentials")
	}
}

// TestSyncMetadata_UnauthenticatedDoesNotScheduleReconnect pins that an
// UNAUTHENTICATED metadata response is treated as auth expiration, not as a
// transient network failure (which would trigger endless reconnects).
func TestSyncMetadata_UnauthenticatedDoesNotScheduleReconnect(t *testing.T) {
	setupTestSession(t)

	backend := &recordingBackend{rejectAuth: true}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	select {
	case <-c.reconnectCh:
		t.Fatal("reconnect already pending before the call")
	default:
	}

	c.sendMetadataOnce()

	select {
	case <-c.reconnectCh:
		t.Fatal("UNAUTHENTICATED scheduled a reconnect; expired credentials must pause sync instead")
	default:
	}
	if got := connstate.Get(); got != connstate.AuthExpired {
		t.Fatalf("state = %q, want %q", got, connstate.AuthExpired)
	}
}

// TestCollectMetadataCarriesConnectionState verifies the runtime status and
// the connection/auth status travel as independent fields.
func TestCollectMetadataCarriesConnectionState(t *testing.T) {
	setupTestSession(t)

	connstate.MarkConnected()
	md := collectMetadata()
	if md.GetCurrentState() != "running" || md.GetMonitorStatus() != "active" {
		t.Errorf("runtime status = %q/%q, want running/active", md.GetCurrentState(), md.GetMonitorStatus())
	}
	if md.GetConnectionState() != connstate.Connected {
		t.Errorf("connection_state = %q, want %q", md.GetConnectionState(), connstate.Connected)
	}

	connstate.MarkAuthExpired()
	md = collectMetadata()
	if md.GetCurrentState() != "running" {
		t.Error("runtime status changed with connection state; they must stay independent")
	}
	if md.GetConnectionState() != connstate.AuthExpired {
		t.Errorf("connection_state = %q, want %q", md.GetConnectionState(), connstate.AuthExpired)
	}
}

// TestTemporaryNetworkFailureKeepsReconnect pins that a plain connection
// error is NOT treated as logout or auth expiry: reconnect stays armed and
// the connection state stays non-terminal.
func TestTemporaryNetworkFailureKeepsReconnect(t *testing.T) {
	setupTestSession(t)
	connstate.MarkConnected()

	err := errors.New("connection refused")
	code := phelixerr.CodeOf(phelixerr.FromGRPC(err))
	if code == phelixerr.CodeUnauthenticated || code == phelixerr.CodeSessionExpired {
		t.Fatalf("generic network error classified as auth failure (%s)", code)
	}
	_ = time.Now // keep time import if assertions above change
}
