package grpc

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// fakeEventBackend accepts any reported application event.
type fakeEventBackend struct {
	pb.UnimplementedPhelixServiceServer
}

func (f *fakeEventBackend) ReportEvent(_ context.Context, _ *pb.ApplicationEvent) (*pb.EventResponse, error) {
	return &pb.EventResponse{Accepted: true}, nil
}

// setupTestSession writes a valid, non-expired session file under a temporary
// HOME so attachAuthMetadata succeeds, and initializes server identity.
func setupTestSession(t *testing.T) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	phelixDir := filepath.Join(tmpHome, ".phelix")
	if err := os.MkdirAll(phelixDir, 0o755); err != nil {
		t.Fatalf("failed to create .phelix dir: %v", err)
	}

	settings := server.Settings{
		Connection: server.ServerConnection{
			SSHPort:    22,
			SSHUser:    "phelix",
			AuthMethod: "key",
		},
		Alert: server.ServerAlert{
			CPUThreshold:  80,
			RAMThreshold:  90,
			DiskThreshold: 90,
		},
		Security: server.ServerSecurity{
			SSHRootLogin: "prohibit-password",
			OpenPorts:    []int32{22},
		},
	}
	settingsData, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("failed to marshal settings: %v", err)
	}
	if err := os.WriteFile(filepath.Join(phelixDir, "settings.json"), settingsData, 0o600); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	session := map[string]any{
		"sessionID": "test-session",
		"token":     "test-token",
		"expiresAt": time.Now().Add(time.Hour),
	}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("failed to marshal session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(phelixDir, "session.json"), data, 0o600); err != nil {
		t.Fatalf("failed to write session file: %v", err)
	}

	if err := server.Initialize(); err != nil {
		t.Fatalf("server.Initialize failed: %v", err)
	}
}

// dialBufconn starts an in-process gRPC server backed by bufconn and returns
// a connected *Client pointed at it.
func dialBufconn(t *testing.T, backend pb.PhelixServiceServer) (*Client, *grpc.Server, func()) {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	pb.RegisterPhelixServiceServer(grpcServer, backend)

	go func() {
		_ = grpcServer.Serve(lis)
	}()

	dialer := func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	}

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufconn: %v", err)
	}

	c := &Client{
		done:        make(chan struct{}),
		reconnectCh: make(chan struct{}, 1),
		reconnect:   newReconnectState(),
	}
	c.conn = conn
	c.serviceClient = pb.NewPhelixServiceClient(conn)
	c.connected = true

	cleanup := func() {
		_ = conn.Close()
		grpcServer.Stop()
		_ = lis.Close()
	}
	return c, grpcServer, cleanup
}

// A CLI command (start/restart/rebuild/remove) reports its result via
// SendEvent, which logs several [INFO] [grpc] lines. Those lines must reach
// the phelix.log file (backend transport) but must NOT echo to stderr — that
// is the bug this test pins. See TestInfoFile_WritesFileNotStderr in the logs
// package.
func TestSendEvent_DoesNotLeakToStderr(t *testing.T) {
	setupTestSession(t)

	// Capture stderr.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = oldStderr }()

	backend := &fakeEventBackend{}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	event := NewApplicationEvent("1", "billing", "start", true, "", 0, "", "")
	c.SendEvent(event)

	_ = w.Close()
	stderrBytes, _ := io.ReadAll(r)
	_ = r.Close()

	if len(stderrBytes) != 0 {
		t.Fatalf("SendEvent leaked %d bytes to stderr:\n%s", len(stderrBytes), string(stderrBytes))
	}
}
