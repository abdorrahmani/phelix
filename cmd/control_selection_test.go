package cmd

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

type selectionClient struct {
	pb.ControlClient
	response *pb.QueryResponse
	err      error
	queries  int
}

func (c *selectionClient) Query(_ context.Context, request *pb.QueryRequest, _ ...grpc.CallOption) (*pb.QueryResponse, error) {
	c.queries++
	if request.Kind != pb.QueryKind_APPLICATIONS {
		return nil, errors.New("unexpected query kind")
	}
	return c.response, c.err
}

func TestControlAppSelectionUsesControllerIDs(t *testing.T) {
	for _, name := range []string{"status", "remove"} {
		t.Run(name, func(t *testing.T) {
			defer machine.LeaveJSON()
			root := &cobra.Command{Use: "phelix"}
			command := &cobra.Command{Use: name}
			flag := map[string]string{"status": "json", "remove": "purge"}[name]
			command.Flags().Bool(flag, false, "")
			if err := command.Flags().Set(flag, "true"); err != nil {
				t.Fatal(err)
			}
			root.AddCommand(command)
			command.SetContext(context.Background())
			client := &selectionClient{response: &pb.QueryResponse{Applications: []*pb.Application{
				{Id: "b", Name: "billing"}, {Id: "unnamed"}, {Id: "a", Name: "billing"},
			}}}
			args, err := selectControlApp(command, nil, client, func(message string, options []string) (string, error) {
				if name == "status" && os.Stdout != os.Stderr {
					t.Fatal("JSON selection printed its menu to stdout")
				}
				if !strings.Contains(message, map[string]string{"status": "inspect", "remove": "remove"}[name]) {
					t.Fatalf("wrong prompt: %s", message)
				}
				if !reflect.DeepEqual(options, []string{"unnamed", "billing (a)", "billing (b)"}) {
					t.Fatalf("incorrect choices: %v", options)
				}
				return options[2], nil
			})
			if err != nil || !reflect.DeepEqual(args, []string{"b"}) || client.queries != 1 {
				t.Fatalf("incorrect selection: %v, %v", args, err)
			}
			if argv := controlArguments(command, args); !reflect.DeepEqual(argv, []string{name, "b", "--" + flag + "=true"}) {
				t.Fatalf("selection lost command flags: %v", argv)
			}
		})
	}
}

func TestControlAppSelectionPreservesExplicitCommands(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		workspace bool
		nested    bool
	}{
		{name: "status", args: []string{"billing"}},
		{name: "remove", args: []string{"app-id"}},
		{name: "status", workspace: true},
		{name: "status", nested: true},
		{name: "list"},
	} {
		t.Run(tc.name+strings.Join(tc.args, ""), func(t *testing.T) {
			root := &cobra.Command{Use: "phelix"}
			command := &cobra.Command{Use: tc.name}
			command.Flags().Bool("workspace", tc.workspace, "")
			parent := root
			if tc.nested {
				parent = &cobra.Command{Use: "auth"}
				root.AddCommand(parent)
			}
			parent.AddCommand(command)
			client := &selectionClient{}
			args, err := selectControlApp(command, tc.args, client, func(string, []string) (string, error) {
				t.Fatal("unexpected prompt")
				return "", nil
			})
			if err != nil || !reflect.DeepEqual(args, tc.args) || client.queries != 0 {
				t.Fatalf("explicit command was changed: %v, %v", args, err)
			}
		})
	}
}

func TestControlAppSelectionStopsOnErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		response  *pb.QueryResponse
		queryErr  error
		choice    string
		chooseErr error
		wantCode  phelixerr.Code
	}{
		{name: "empty", response: &pb.QueryResponse{}, wantCode: phelixerr.CodeNotFound},
		{name: "unavailable", queryErr: errors.New("Controller unavailable")},
		{name: "cancelled", response: &pb.QueryResponse{Applications: []*pb.Application{{Id: "a"}}}, chooseErr: errors.New("selection cancelled")},
		{name: "invalid", response: &pb.QueryResponse{Applications: []*pb.Application{{Id: "a"}}}, choice: "missing", wantCode: phelixerr.CodeInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := &cobra.Command{Use: "phelix"}
			command := &cobra.Command{Use: "remove"}
			root.AddCommand(command)
			command.SetContext(context.Background())
			client := &selectionClient{response: tc.response, err: tc.queryErr}
			args, err := selectControlApp(command, nil, client, func(string, []string) (string, error) {
				if tc.queryErr != nil || len(tc.response.Applications) == 0 {
					t.Fatal("prompted without an app list")
				}
				return tc.choice, tc.chooseErr
			})
			if err == nil || len(args) != 0 {
				t.Fatalf("selection error was ignored: %v, %v", args, err)
			}
			if tc.wantCode != "" && phelixerr.CodeOf(err) != tc.wantCode {
				t.Fatalf("incorrect error: %v", err)
			}
		})
	}
}

func TestControlAppSelectionCoversApplicationCommands(t *testing.T) {
	for _, tc := range []struct {
		path string
		use  string
		args []string
		want []string
	}{
		{path: "rebuild", use: "rebuild [ID|AppName] --port <PORT>", want: []string{"app-id"}},
		{path: "deploy", use: "deploy [ID|AppName]", want: []string{"app-id"}},
		{path: "start", use: "start [ID|AppName]", want: []string{"app-id"}},
		{path: "stop", use: "stop [ID|AppName]", want: []string{"app-id"}},
		{path: "restart", use: "restart [ID|AppName]", want: []string{"app-id"}},
		{path: "watch", use: "watch <ID|AppName>", want: []string{"app-id"}},
		{path: "scale", use: "scale [ID|AppName] --replicas N", want: []string{"app-id"}},
		{path: "rollback", use: "rollback [AppName]", want: []string{"billing"}},
		{path: "rollback history", use: "history [AppName]", want: []string{"billing"}},
		{path: "deploy unlock", use: "unlock [AppName]", want: []string{"billing"}},
		{path: "dockerize", use: "dockerize <AppName> [--tag v1]", want: []string{"billing"}},
		{path: "build-report", use: "build-report [AppName]", want: []string{"billing"}},
		{path: "deployment-report", use: "deployment-report [AppName]", want: []string{"billing"}},
		{path: "health add", use: "add [ID|AppName]", want: []string{"app-id"}},
		{path: "autoscale enable", use: "enable [ID|AppName]", want: []string{"app-id"}},
		{path: "hooks inspect", use: "inspect <execution-id> [ID|AppName]", args: []string{"execution-id"}, want: []string{"execution-id", "app-id"}},
		{path: "secrets set", use: "set NAME [ID|AppName]", args: []string{"TOKEN"}, want: []string{"TOKEN", "app-id"}},
		{path: "inspect deployment", use: "deployment [ID|AppName]", want: []string{"app-id"}},
		{path: "plan create rebuild", use: "rebuild [ID|AppName]", want: []string{"app-id"}},
		{path: "job run", use: "run <NAME>", want: []string{"app-id"}},
		{path: "context", use: "context [ID|AppName]", want: []string{"app-id"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			command := selectionCommand(tc.path, tc.use)
			client := &selectionClient{response: &pb.QueryResponse{Applications: []*pb.Application{{Id: "app-id", Name: "billing"}}}}
			args, err := selectControlApp(command, tc.args, client, func(_ string, options []string) (string, error) {
				return options[len(options)-1], nil
			})
			if err != nil || !reflect.DeepEqual(args, tc.want) || client.queries != 1 {
				t.Fatalf("selection = %v, %v; queries = %d", args, err, client.queries)
			}
		})
	}
}

func TestControlAppSelectionExcludesSecurityAndLog(t *testing.T) {
	for _, path := range []string{"security scan", "security supply-chain verify", "log"} {
		command := selectionCommand(path, strings.Fields(path)[len(strings.Fields(path))-1]+" [ID|AppName]")
		client := &selectionClient{}
		_, err := selectControlApp(command, nil, client, func(string, []string) (string, error) {
			t.Fatal("excluded command prompted")
			return "", nil
		})
		if err != nil || client.queries != 0 {
			t.Fatalf("excluded command queried applications: %v", err)
		}
	}
}

func TestControlAppSelectionPreservesAllApplications(t *testing.T) {
	command := selectionCommand("start", "start [ID|AppName]")
	client := &selectionClient{response: &pb.QueryResponse{Applications: []*pb.Application{{Id: "app-id"}}}}
	args, err := selectControlApp(command, nil, client, func(_ string, options []string) (string, error) {
		return options[0], nil
	})
	if err != nil || len(args) != 0 {
		t.Fatalf("start all = %v, %v", args, err)
	}
	for _, flag := range []string{"all", "workspace"} {
		command := selectionCommand("rebuild", "rebuild [ID|AppName]")
		command.Flags().Bool(flag, true, "")
		client := &selectionClient{}
		_, err := selectControlApp(command, nil, client, func(string, []string) (string, error) {
			t.Fatal("workspace operation prompted")
			return "", nil
		})
		if err != nil || client.queries != 0 {
			t.Fatalf("workspace operation queried applications: %v", err)
		}
	}
}

func selectionCommand(path, use string) *cobra.Command {
	root := &cobra.Command{Use: "phelix"}
	parent := root
	parts := strings.Fields(path)
	for _, name := range parts[:len(parts)-1] {
		child := &cobra.Command{Use: name}
		parent.AddCommand(child)
		parent = child
	}
	command := &cobra.Command{Use: use}
	parent.AddCommand(command)
	command.SetContext(context.Background())
	return command
}
