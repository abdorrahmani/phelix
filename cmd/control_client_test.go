package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/abdorrahmani/phelix/internal/controlapi"
	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	"github.com/spf13/cobra"
)

func TestControlRoutesEveryManagementFamily(t *testing.T) {
	names := []string{"build", "dockerize", "rebuild", "deploy", "rollback", "start", "stop", "restart", "remove", "status", "list", "log", "watch", "scale", "autoscale", "job", "hooks", "env", "secrets", "health", "proxy", "matrix", "operation", "inspect", "plan", "authz", "context", "session", "security", "webhook", "build-report", "deployment-report", "config"}
	socket := filepath.Join(t.TempDir(), "api.sock")
	t.Setenv("PHELIX_CONTROL_SOCKET", socket)
	requests := make(chan *pb.CommandRequest, len(names))
	m, err := controlapi.NewOperations(context.Background(), t.TempDir(), 2, func(ctx context.Context, r *pb.CommandRequest, p func(string, bool)) (int, error) {
		requests <- r
		p("done\n", false)
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	server, err := controlapi.Listen(socket, []uint32{uint32(os.Getuid())}, &controlapi.Service{Operations: m, Validate: func(r *pb.CommandRequest) (bool, error) { return false, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			root := &cobra.Command{Use: "phelix"}
			local := false
			command := &cobra.Command{Use: name, RunE: func(*cobra.Command, []string) error { local = true; return fmt.Errorf("local handler executed") }}
			command.Flags().String("tag", "", "tag")
			root.AddCommand(command)
			RouteControl(root)
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetArgs([]string{name, "sample", "--tag=release"})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if local {
				t.Fatal("business logic ran in CLI")
			}
			request := <-requests
			if len(request.Argv) != 3 || request.Argv[0] != name || request.Argv[2] != "--tag=release" {
				t.Fatal(request)
			}
			if output.String() != "done\n" {
				t.Fatal(output.String())
			}
		})
	}
}

func TestControlLeavesDeveloperHelpersLocal(t *testing.T) {
	for _, name := range []string{"init", "doctor", "completion", "version"} {
		root := &cobra.Command{Use: "phelix"}
		called := false
		root.AddCommand(&cobra.Command{Use: name, RunE: func(*cobra.Command, []string) error { called = true; return nil }})
		RouteControl(root)
		root.SetArgs([]string{name})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if !called {
			t.Fatal(name, "not local")
		}
	}
}
