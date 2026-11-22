package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/env"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type envEventBackend struct {
	pb.UnimplementedPhelixServiceServer
	events atomic.Int32
}

func (b *envEventBackend) ReportEvent(ctx context.Context, event *pb.ApplicationEvent) (*pb.EventResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if strings.Join(md.Get("authorization"), "") != "Bearer test-token" || strings.Join(md.Get("sessionid"), "") != "test-session" {
		return nil, status.Error(codes.Unauthenticated, "missing test session")
	}
	if event.GetAction() != "env_change" || !event.GetSuccess() || event.GetAppName() != "notification" {
		return nil, status.Error(codes.InvalidArgument, "unexpected environment event")
	}
	b.events.Add(1)
	select {
	case <-time.After(100 * time.Millisecond):
		return &pb.EventResponse{Accepted: true}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestCLIEnvironmentReportsOncePerUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real CLI and Controller")
	}
	binary := phelixBin(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend := &envEventBackend{}
	server := grpc.NewServer()
	pb.RegisterPhelixServiceServer(server, backend)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	bulk := []string{"EMPTY="}
	for i := 0; i < 65; i++ {
		bulk = append(bulk, fmt.Sprintf("KEY_%d=private value=with spaces", i))
	}
	for _, tc := range []struct {
		name       string
		values     []string
		wantKeys   int
		wantEvents int32
		wantError  bool
	}{
		{"bulk", bulk, 66, 1, false},
		{"single", []string{"TOKEN=private value=with spaces"}, 1, 1, false},
		{"partial", []string{"TOKEN=private value=with spaces", "invalid"}, 1, 1, true},
		{"invalid", []string{"invalid"}, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			directory := filepath.Join(home, ".phelix")
			t.Setenv("HOME", home)
			t.Setenv("PHELIX_DATA_DIR", directory)
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			registry := `{"50102030-4050-4670-8090-aabbccddeeff":{"ID":"50102030-4050-4670-8090-aabbccddeeff","Name":"notification","Status":"stopped","Watching":true}}`
			session, err := json.Marshal(map[string]any{"sessionID": "test-session", "token": "test-token", "expiresAt": time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string][]byte{"apps.json": []byte(registry), "session.json": session} {
				if err := os.WriteFile(filepath.Join(directory, name), content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, append([]string{"env", "set", "notification"}, tc.values...)...)
			command.Env = append(os.Environ(), "PHELIX_MODE=dev", "PHELIX_GRPC_URL="+listener.Addr().String())
			prepareControlTest(t, command)
			before := backend.events.Load()
			output, runErr := command.CombinedOutput()
			if ctx.Err() != nil || (runErr != nil) != tc.wantError {
				t.Fatalf("environment update error = %v, deadline = %v", runErr, ctx.Err())
			}
			if got := backend.events.Load() - before; got != tc.wantEvents {
				t.Fatalf("reported %d events, want %d", got, tc.wantEvents)
			}
			if strings.Contains(string(output), "private value") {
				t.Fatal("environment values appeared in command output")
			}
			values, err := env.Instance.GetAllEnvVars("50102030-4050-4670-8090-aabbccddeeff")
			if err != nil || len(values) != tc.wantKeys {
				t.Fatalf("stored keys = %d, want %d, error = %v", len(values), tc.wantKeys, err)
			}
			for key, value := range values {
				if key == "EMPTY" && value != "" || key != "EMPTY" && value != "private value=with spaces" {
					t.Fatal("stored environment value changed")
				}
			}
		})
	}
}
