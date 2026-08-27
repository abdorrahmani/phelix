package grpc

import (
	"context"
	"strings"
	"sync"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/server"
)

// removeCaptureBackend records every ApplicationEvent it receives so tests can
// assert exactly what crossed the wire for CLI-initiated removals.
type removeCaptureBackend struct {
	pb.UnimplementedPhelixServiceServer

	mu     sync.Mutex
	events []*pb.ApplicationEvent
	resp   *pb.EventResponse
}

func (b *removeCaptureBackend) ReportEvent(_ context.Context, ev *pb.ApplicationEvent) (*pb.EventResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.events == nil {
		b.events = make([]*pb.ApplicationEvent, 0)
	}
	b.events = append(b.events, ev)
	if b.resp != nil {
		return b.resp, nil
	}
	return &pb.EventResponse{Accepted: true}, nil
}

func (b *removeCaptureBackend) recorded() []*pb.ApplicationEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*pb.ApplicationEvent, len(b.events))
	copy(out, b.events)
	return out
}

// A successful `phelix remove` must reach the backend as an
// ApplicationEvent{action: "remove"} carrying the server (agent) identity and
// the app's CLI identity — this is the signal the backend uses to delete the
// app row from its database.
func TestSendEventChecked_RemoveEventDeliveredToBackend(t *testing.T) {
	setupTestSession(t)

	backend := &removeCaptureBackend{}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	serverID := server.GetServerID()
	err := c.SendEventChecked(NewApplicationEvent(
		"75b4d7e5-7e34-4641-9c2e-e3f54e092f75", "billing", "remove", true, "", 0, "", "",
	))
	if err != nil {
		t.Fatalf("SendEventChecked returned error for accepted event: %v", err)
	}

	got := backend.recorded()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 reported event, got %d", len(got))
	}

	ev := got[0]
	if ev.GetAction() != "remove" {
		t.Errorf("event action = %q, want %q", ev.GetAction(), "remove")
	}
	if ev.GetAppId() != "75b4d7e5-7e34-4641-9c2e-e3f54e092f75" {
		t.Errorf("event app_id = %q, want the CLI app id", ev.GetAppId())
	}
	if ev.GetAppName() != "billing" {
		t.Errorf("event app_name = %q, want %q", ev.GetAppName(), "billing")
	}
	if !ev.GetSuccess() {
		t.Error("event success = false, want true for a successful removal")
	}
	if ev.GetServerId() == "" || ev.GetServerId() != serverID {
		t.Errorf("event server_id = %q, want the agent id %q", ev.GetServerId(), serverID)
	}
	if ev.ErrorMessage != "" || ev.Pid != 0 || ev.DeploymentMode != "" || ev.Version != "" {
		t.Errorf("pid/deployment_mode/version/error_message should be zero for remove, got %+v", ev)
	}
}

// When the CLI has no connection (and the monitor daemon isn't running through
// a temporary dial), the caller must learn that nothing was delivered instead
// of assuming the dashboard was updated.
func TestSendEventChecked_NotConnectedReturnsError(t *testing.T) {
	setupTestSession(t)

	c := NewClient()
	err := c.SendEventChecked(NewApplicationEvent("1", "billing", "remove", true, "", 0, "", ""))
	if err == nil {
		t.Fatal("SendEventChecked on a disconnected client must return an error")
	}
	if code := phelixerr.CodeOf(err); code != phelixerr.CodeConnection {
		t.Errorf("error code = %v, want %v", code, phelixerr.CodeConnection)
	}
}

// A backend that refuses the event (accepted=false) must surface its reason to
// the caller — that refusal is how a stale/misrouted removal becomes visible.
func TestSendEventChecked_RejectedByBackendSurfacesMessage(t *testing.T) {
	setupTestSession(t)

	backend := &removeCaptureBackend{
		resp: &pb.EventResponse{Accepted: false, Message: "unknown server_id"},
	}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	err := c.SendEventChecked(NewApplicationEvent("1", "billing", "remove", true, "", 0, "", ""))
	if err == nil {
		t.Fatal("SendEventChecked must fail when the backend sets accepted=false")
	}
	if !strings.Contains(err.Error(), "unknown server_id") {
		t.Errorf("error %q should carry the backend's rejection message", err.Error())
	}
}

// Without a session the report cannot even be attributed; the command's local
// work still stands, but the caller gets an actionable error rather than the
// historical silent skip.
func TestReportEventResult_NoSessionReturnsActionableError(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.phelix/session.json

	err := ReportEventResult("1", "billing", "remove", true, "", 0, "", "")
	if err == nil {
		t.Fatal("ReportEventResult without a session must return an error, not silently drop the event")
	}
	if code := phelixerr.CodeOf(err); code != phelixerr.CodeUnauthenticated {
		t.Errorf("error code = %v, want %v", code, phelixerr.CodeUnauthenticated)
	}
	if !strings.Contains(err.Error(), "phelix auth login") {
		t.Errorf("error %q should tell the user how to fix it", err.Error())
	}
}
