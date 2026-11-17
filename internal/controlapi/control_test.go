package controlapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestOperationLifecycleCancellationAndRestart(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	m, err := NewOperations(context.Background(), dir, 2, func(ctx context.Context, request *pb.CommandRequest, progress func(string, bool)) (int, error) {
		progress("building\n", false)
		close(started)
		<-ctx.Done()
		return 1, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.Submit(&pb.CommandRequest{}, true)
	if err != nil || op.State != pb.OperationState_QUEUED {
		t.Fatal(op, err)
	}
	<-started
	if _, err := m.Cancel(op.Id); err != nil {
		t.Fatal(err)
	}
	var last *pb.Operation
	if err := m.Watch(context.Background(), op.Id, 0, func(p *pb.Progress) error { last = p.Operation; return nil }); err != nil {
		t.Fatal(err)
	}
	if last.State != pb.OperationState_CANCELLED {
		t.Fatal(last)
	}
	m.Close()
	recovered, err := NewOperations(context.Background(), dir, 1, func(context.Context, *pb.CommandRequest, func(string, bool)) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	loaded, err := recovered.Get(op.Id)
	if err != nil || loaded.State != pb.OperationState_CANCELLED {
		t.Fatal(loaded, err)
	}
}

func TestOperationsConcurrentErrorsAndUnsafeCancel(t *testing.T) {
	var mu sync.Mutex
	active, maximum := 0, 0
	gate := make(chan struct{})
	m, err := NewOperations(context.Background(), t.TempDir(), 2, func(ctx context.Context, r *pb.CommandRequest, p func(string, bool)) (int, error) {
		mu.Lock()
		active++
		if active > maximum {
			maximum = active
		}
		mu.Unlock()
		<-gate
		mu.Lock()
		active--
		mu.Unlock()
		return 20, errors.New("build failed")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var ids []string
	for range 3 {
		op, err := m.Submit(&pb.CommandRequest{}, false)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, op.Id)
	}
	if _, err := m.Cancel(ids[0]); status.Code(err) != codes.FailedPrecondition {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		ready := maximum == 2
		mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("operations did not execute concurrently")
		}
		time.Sleep(time.Millisecond)
	}
	close(gate)
	for _, id := range ids {
		err := m.Watch(context.Background(), id, 0, func(p *pb.Progress) error {
			if terminal(p.Operation.State) && p.Operation.State != pb.OperationState_FAILED {
				t.Fatal(p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		op, err := m.Get(id)
		if err != nil || op.Error == nil || op.ExitCode != 20 {
			t.Fatal(op, err)
		}
	}
}

func TestUnixTransportPermissionsInvalidRequestAndReconnect(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	manager, err := NewOperations(context.Background(), t.TempDir(), 1, func(context.Context, *pb.CommandRequest, func(string, bool)) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	service := &Service{Operations: manager, InfoResponse: &pb.ControllerInfo{AgentId: "stable"}, Validate: func(r *pb.CommandRequest) (bool, error) { return false, errors.New("invalid command") }}
	for range 2 {
		server, err := Listen(socket, []uint32{uint32(os.Getuid())}, service)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(socket)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal(info, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn, client, err := Connect(ctx, socket)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Info(ctx, &pb.Empty{})
		if err != nil || response.AgentId != "stable" {
			t.Fatal(response, err)
		}
		if _, err := client.Execute(ctx, &pb.CommandRequest{}); status.Code(err) != codes.InvalidArgument {
			t.Fatal(err)
		}
		if other, err := Listen(socket, []uint32{uint32(os.Getuid())}, service); err == nil {
			other.Close()
			t.Fatal("live socket replaced")
		}
		conn.Close()
		cancel()
		server.Close()
		if _, err := os.Stat(socket); !os.IsNotExist(err) {
			t.Fatal("socket not cleaned", err)
		}
	}
	os.WriteFile(socket, []byte("keep"), 0600)
	if server, err := Listen(socket, nil, service); err == nil {
		server.Close()
		t.Fatal("regular file replaced")
	}
}

func TestUnixUnauthorizedPeer(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	server, err := Listen(socket, []uint32{uint32(os.Getuid()) + 1000}, &Service{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if conn, _, err := Connect(ctx, socket); err == nil {
		conn.Close()
		t.Fatal("unauthorized peer accepted")
	}
}

func TestInterruptedOperationRecovery(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "id.json"), []byte(`{"id":"id","state":"RUNNING"}`), 0600)
	m, err := NewOperations(context.Background(), dir, 1, func(context.Context, *pb.CommandRequest, func(string, bool)) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	op, err := m.Get("id")
	if err != nil || op.State != pb.OperationState_INTERRUPTED || op.Error == nil {
		t.Fatal(op, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Watch(ctx, "missing", 0, func(*pb.Progress) error { return nil }); status.Code(err) != codes.NotFound {
		t.Fatal(err)
	}
}
