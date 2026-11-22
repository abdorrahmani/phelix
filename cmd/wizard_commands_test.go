package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/controlapi"
	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/spf13/cobra"
)

func TestWizardCompletesArgumentsBeforeValidationAndSubmission(t *testing.T) {
	for _, tc := range []struct {
		path     string
		use      string
		validate cobra.PositionalArgs
		args     []string
		values   []string
		want     []string
		flags    []string
		secret   string
	}{
		{path: "rebuild", use: "rebuild [ID|AppName]", validate: cobra.MaximumNArgs(1), want: []string{"rebuild", "app-id"}},
		{path: "watch", use: "watch <ID|AppName>", validate: cobra.ExactArgs(1), want: []string{"watch", "app-id"}},
		{path: "hooks inspect", use: "inspect <execution-id> [ID|AppName]", validate: cobra.RangeArgs(1, 2), values: []string{"execution-id"}, want: []string{"hooks", "inspect", "execution-id", "app-id"}},
		{path: "health add", use: "add [ID|AppName]", validate: cobra.MaximumNArgs(1), flags: []string{"name", "url"}, values: []string{"ready", "http://localhost:8080/health"}, want: []string{"health", "add", "app-id", "--name=ready", "--url=http://localhost:8080/health"}},
		{path: "secrets set", use: "set NAME [ID|AppName]", validate: cobra.RangeArgs(1, 2), values: []string{"TOKEN"}, secret: "private test value", want: []string{"secrets", "set", "TOKEN", "app-id"}},
		{path: "env", use: EnvCmd.Use, validate: cobra.MinimumNArgs(0), args: []string{"get"}, values: []string{"TOKEN"}, want: []string{"env", "get", "app-id", "TOKEN"}},
		{path: "job run", use: "run <NAME>", validate: cobra.ExactArgs(1), want: []string{"job", "run", "app-id"}},
		{path: "session show", use: "show <session-id>", validate: cobra.ExactArgs(1), values: []string{"ses-demo"}, want: []string{"session", "show", "ses-demo"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			requests := wizardTestController(t, &pb.QueryResponse{Applications: []*pb.Application{{Id: "app-id", Name: "billing"}}})
			command := selectionCommand(tc.path, tc.use)
			command.Args = tc.validate
			command.RunE = func(*cobra.Command, []string) error { t.Fatal("ran local action"); return nil }
			for _, flag := range tc.flags {
				command.Flags().String(flag, "", flag)
				if err := command.MarkFlagRequired(flag); err != nil {
					t.Fatal(err)
				}
			}
			if tc.secret != "" {
				command.Flags().String("from-file", "", "")
			}
			RouteControl(command.Root())
			remaining := append([]string{}, tc.values...)
			configureWizards(command.Root(), func() bool { return true }, func(_ string, options []string) (string, error) {
				return options[len(options)-1], nil
			}, func(_ string, def string) (string, error) {
				if len(remaining) == 0 {
					t.Fatal("unexpected text prompt")
					return "", nil
				}
				value := remaining[0]
				remaining = remaining[1:]
				return value, nil
			}, func(string) (string, error) { return tc.secret, nil })
			command.Root().SetArgs(append(strings.Fields(tc.path), tc.args...))
			command.Root().SetOut(&bytes.Buffer{})
			if err := command.Root().Execute(); err != nil {
				t.Fatal(err)
			}
			request := <-requests
			if !reflect.DeepEqual(request.Argv, tc.want) || string(request.Input) != tc.secret || len(remaining) != 0 {
				t.Fatalf("request = %v, input length = %d, unused answers = %v", request.Argv, len(request.Input), remaining)
			}
		})
	}
}

func TestWizardPreservesExplicitArgumentsAndFlags(t *testing.T) {
	requests := wizardTestController(t, nil)
	command := selectionCommand("rebuild", "rebuild [ID|AppName]")
	command.Args = cobra.MaximumNArgs(1)
	command.RunE = runControlClient
	command.Flags().StringArray("build-arg", nil, "")
	command.Flags().Bool("json", false, "")
	configureWizards(command.Root(), func() bool { return true }, rejectWizardChoice(t), rejectWizardInput(t), rejectWizardPassword(t))
	command.Root().SetArgs([]string{"rebuild", "billing", "--build-arg=-trimpath", "--build-arg=-ldflags=-s -w", "--json"})
	command.Root().SetOut(&bytes.Buffer{})
	if err := command.Root().Execute(); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	want := []string{"rebuild", "billing", "--build-arg=-trimpath", "--build-arg=-ldflags=-s -w", "--json=true"}
	if !reflect.DeepEqual(request.Argv, want) {
		t.Fatalf("arguments changed: %v", request.Argv)
	}
}

func TestWizardKeepsScriptsAndExceptionsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		path        string
		interactive bool
	}{
		{path: "watch"}, {path: "security scan", interactive: true}, {path: "log", interactive: true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			command := selectionCommand(tc.path, strings.Fields(tc.path)[len(strings.Fields(tc.path))-1]+" <ID|AppName>")
			command.Args = cobra.ExactArgs(1)
			called := false
			command.RunE = func(*cobra.Command, []string) error { called = true; return nil }
			configureWizards(command.Root(), func() bool { return tc.interactive }, rejectWizardChoice(t), rejectWizardInput(t), rejectWizardPassword(t))
			command.Root().SetArgs(strings.Fields(tc.path))
			command.Root().SilenceErrors, command.Root().SilenceUsage = true, true
			if err := command.Root().Execute(); err == nil || called {
				t.Fatal("missing argument was accepted")
			}
		})
	}
}

func TestWizardCancellationPreventsExecution(t *testing.T) {
	command := selectionCommand("session show", "show <session-id>")
	command.Args = cobra.ExactArgs(1)
	called := false
	command.RunE = func(*cobra.Command, []string) error { called = true; return nil }
	cancelled := errors.New("input cancelled")
	configureWizards(command.Root(), func() bool { return true }, rejectWizardChoice(t), func(string, string) (string, error) { return "", cancelled }, rejectWizardPassword(t))
	command.Root().SetArgs([]string{"session", "show"})
	command.Root().SilenceErrors, command.Root().SilenceUsage = true, true
	if err := command.Root().Execute(); !errors.Is(err, cancelled) || called {
		t.Fatalf("cancellation = %v; executed = %t", err, called)
	}
}

func TestWizardMenusCoverCommandsAndUseTheirValidation(t *testing.T) {
	root := &cobra.Command{Use: "phelix"}
	group := &cobra.Command{Use: "session"}
	called, before, after := false, false, false
	leaf := &cobra.Command{Use: "show <session-id>", Short: "Show a session", Args: cobra.ExactArgs(1),
		PreRun:  func(*cobra.Command, []string) { before = true },
		PostRun: func(*cobra.Command, []string) { after = true },
		RunE: func(_ *cobra.Command, args []string) error {
			called = reflect.DeepEqual(args, []string{"ses-demo"})
			return nil
		},
	}
	group.AddCommand(leaf)
	root.AddCommand(group)
	for _, name := range []string{"security", "log", "wizard"} {
		root.AddCommand(&cobra.Command{Use: name, RunE: func(*cobra.Command, []string) error { t.Fatal("excluded task ran"); return nil }})
	}
	choices := []string{"session", "show"}
	configureWizards(root, func() bool { return true }, func(_ string, options []string) (string, error) {
		for _, option := range options {
			if strings.HasPrefix(option, "security") || strings.HasPrefix(option, "log:") || strings.HasPrefix(option, "wizard") {
				t.Fatalf("excluded task offered: %v", options)
			}
		}
		if len(choices) == 0 {
			t.Fatal("unexpected menu")
			return "", nil
		}
		choice := choices[0]
		choices = choices[1:]
		for _, option := range options {
			if strings.HasPrefix(option, choice) {
				return option, nil
			}
		}
		t.Fatalf("task %s missing from %v", choice, options)
		return "", nil
	}, func(string, string) (string, error) { return "ses-demo", nil }, rejectWizardPassword(t))
	root.SetArgs([]string{})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !called || !before || !after || len(choices) != 0 {
		t.Fatalf("action = %t, before = %t, after = %t, choices = %v", called, before, after, choices)
	}
}

func TestWizardBuildUsesProjectDefaults(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("phelix.yaml", []byte("name: billing\nport: 8090\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := selectionCommand("build", BuildCmd.Use)
	command.Flags().Int("port", 8080, "Application port")
	args, err := prepareWizardCommand(command, nil, rejectWizardChoice(t), func(_ string, def string) (string, error) {
		if def != "billing" {
			t.Fatalf("wrong default: %s", def)
		}
		return def, nil
	}, rejectWizardPassword(t))
	if err != nil || !reflect.DeepEqual(args, []string{"billing"}) || command.Flags().Changed("port") {
		t.Fatalf("project defaults = %v, %v", args, err)
	}
}

func TestWizardValidatesRequiredFlagValues(t *testing.T) {
	command := selectionCommand("scale", ScaleCmd.Use)
	command.Flags().Int("replicas", 0, "Replica count")
	_, err := prepareWizardCommand(command, []string{"billing"}, rejectWizardChoice(t), func(string, string) (string, error) { return "invalid", nil }, rejectWizardPassword(t))
	if err == nil {
		t.Fatal("invalid replica count was accepted")
	}
}

func TestWizardRollbackRequiresConfirmation(t *testing.T) {
	for _, answer := range []string{"Cancel", "Proceed"} {
		t.Run(answer, func(t *testing.T) {
			requests := wizardTestController(t, &pb.QueryResponse{Versions: []*pb.Version{{Number: 2, Current: true}, {Number: 1, Tag: "previous"}}})
			command := selectionCommand("rollback", RollbackCmd.Use)
			command.Args = cobra.MaximumNArgs(1)
			command.RunE = runControlClient
			command.Flags().String("to", "", "")
			command.Flags().String("reason", "", "")
			command.Flags().Bool("dry-run", false, "")
			command.Flags().Bool("list", false, "")
			configureWizards(command.Root(), func() bool { return true }, func(message string, options []string) (string, error) {
				if strings.HasPrefix(message, "Proceed") {
					return answer, nil
				}
				if !reflect.DeepEqual(options, []string{"v1 (previous)"}) {
					t.Fatalf("rollback choices = %v", options)
				}
				return options[0], nil
			}, func(string, string) (string, error) { return "", nil }, rejectWizardPassword(t))
			command.Root().SetArgs([]string{"rollback", "billing"})
			command.Root().SetOut(&bytes.Buffer{})
			if err := command.Root().Execute(); err != nil {
				t.Fatal(err)
			}
			preview := <-requests
			if !reflect.DeepEqual(preview.Argv, []string{"rollback", "billing", "--dry-run=true", "--to=1"}) {
				t.Fatalf("preview = %v", preview.Argv)
			}
			if answer == "Proceed" {
				request := <-requests
				if !reflect.DeepEqual(request.Argv, []string{"rollback", "billing", "--to=1"}) {
					t.Fatalf("rollback = %v", request.Argv)
				}
			} else {
				select {
				case request := <-requests:
					t.Fatalf("cancelled rollback submitted: %v", request.Argv)
				default:
				}
			}
		})
	}
}

func TestWizardRollbackWithNoPreviousVersionsDoesNothing(t *testing.T) {
	requests := wizardTestController(t, &pb.QueryResponse{Versions: []*pb.Version{{Number: 1, Current: true}}})
	command := selectionCommand("rollback", RollbackCmd.Use)
	command.Args = cobra.MaximumNArgs(1)
	command.RunE = runControlClient
	command.Flags().String("to", "", "")
	command.Flags().Bool("dry-run", false, "")
	command.Flags().Bool("list", false, "")
	configureWizards(command.Root(), func() bool { return true }, rejectWizardChoice(t), rejectWizardInput(t), rejectWizardPassword(t))
	command.Root().SetArgs([]string{"rollback", "billing"})
	command.Root().SetOut(&bytes.Buffer{})
	if err := command.Root().Execute(); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requests:
		t.Fatalf("empty rollback submitted: %v", request.Argv)
	default:
	}
}

func TestWizardRejectsEmptyRequiredInput(t *testing.T) {
	command := selectionCommand("session show", "show <session-id>")
	_, err := prepareWizardCommand(command, nil, rejectWizardChoice(t), func(string, string) (string, error) { return "", nil }, rejectWizardPassword(t))
	if phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
		t.Fatalf("empty input error = %v", err)
	}
}

func wizardTestController(t *testing.T, response *pb.QueryResponse) <-chan *pb.CommandRequest {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "control.sock")
	t.Setenv("PHELIX_CONTROL_SOCKET", socket)
	requests := make(chan *pb.CommandRequest, 8)
	operations, err := controlapi.NewOperations(context.Background(), t.TempDir(), 2, func(_ context.Context, request *pb.CommandRequest, _ func(string, bool)) (int, error) {
		requests <- request
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { operations.Close() })
	listener, err := controlapi.Listen(socket, []uint32{uint32(os.Getuid())}, &controlapi.Service{
		Operations: operations,
		Validate:   func(*pb.CommandRequest) (bool, error) { return false, nil },
		QueryHandler: func(context.Context, *pb.QueryRequest) (*pb.QueryResponse, error) {
			if response == nil {
				return nil, errors.New("unexpected application query")
			}
			return response, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return requests
}

func rejectWizardChoice(t *testing.T) func(string, []string) (string, error) {
	t.Helper()
	return func(message string, _ []string) (string, error) {
		t.Errorf("unexpected menu: %s", message)
		return "", errors.New("unexpected menu")
	}
}

func rejectWizardInput(t *testing.T) func(string, string) (string, error) {
	t.Helper()
	return func(message, _ string) (string, error) {
		t.Errorf("unexpected input: %s", message)
		return "", errors.New("unexpected input")
	}
}

func rejectWizardPassword(t *testing.T) func(string) (string, error) {
	t.Helper()
	return func(message string) (string, error) {
		t.Errorf("unexpected private input: %s", message)
		return "", errors.New("unexpected private input")
	}
}

func TestWizardAuthenticationCompletesMissingCredentials(t *testing.T) {
	command := selectionCommand("auth login", "login --username <username> --apikey <apikey>")
	command.Flags().String("username", "", "")
	command.Flags().String("apiKey", "", "")
	if err := command.Flags().Set("username", "operator"); err != nil {
		t.Fatal(err)
	}
	_, err := prepareWizardCommand(command, nil, rejectWizardChoice(t), rejectWizardInput(t), func(string) (string, error) { return "private-test-key", nil })
	if err != nil {
		t.Fatal(err)
	}
	username, _ := command.Flags().GetString("username")
	key, _ := command.Flags().GetString("apiKey")
	if username != "operator" || key != "private-test-key" {
		t.Fatal("missing credential was not completed")
	}
}

func TestWizardSecretFileDoesNotReadTerminalInput(t *testing.T) {
	requests := wizardTestController(t, nil)
	command := selectionCommand("secrets set", "set NAME [ID|AppName]")
	command.RunE = runControlClient
	command.Flags().String("from-file", "", "Secret file")
	command.SetIn(strings.NewReader("unread input"))
	configureWizards(command.Root(), func() bool { return true }, rejectWizardChoice(t), rejectWizardInput(t), rejectWizardPassword(t))
	command.Root().SetArgs([]string{"secrets", "set", "TOKEN", "billing", "--from-file=value.txt"})
	command.Root().SetOut(&bytes.Buffer{})
	if err := command.Root().Execute(); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	if len(request.Input) != 0 {
		t.Fatal("file input unexpectedly read stdin")
	}
}

func TestWizardWorkspaceAndResumeDoNotPrompt(t *testing.T) {
	for _, flag := range []string{"all", "workspace", "resume"} {
		command := selectionCommand("build", BuildCmd.Use)
		if flag == "resume" {
			command.Flags().String(flag, "latest", "")
		} else {
			command.Flags().Bool(flag, true, "")
		}
		_, err := prepareWizardCommand(command, nil, rejectWizardChoice(t), rejectWizardInput(t), rejectWizardPassword(t))
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestWizardMenusPreserveInheritedFlags(t *testing.T) {
	root := &cobra.Command{Use: "phelix"}
	root.PersistentFlags().StringArray("label", nil, "")
	group := &cobra.Command{Use: "session"}
	leaf := &cobra.Command{Use: "list", RunE: func(c *cobra.Command, _ []string) error {
		values, err := c.Flags().GetStringArray("label")
		if err != nil || !reflect.DeepEqual(values, []string{"one", "two"}) {
			t.Fatalf("inherited flags = %v, %v", values, err)
		}
		return nil
	}}
	group.AddCommand(leaf)
	root.AddCommand(group)
	configureWizards(root, func() bool { return true }, func(_ string, options []string) (string, error) {
		for _, option := range options {
			if option == "session" || option == "list" {
				return option, nil
			}
		}
		t.Fatalf("missing menu choice in %v", options)
		return "", nil
	}, rejectWizardInput(t), rejectWizardPassword(t))
	root.SetArgs([]string{"session", "--label=one", "--label=two"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestWizardCheckpointCollectsMissingDetails(t *testing.T) {
	command := selectionCommand("session checkpoint", sessionCheckpointCmd.Use)
	command.Flags().String("note", "", "Checkpoint note")
	command.Flags().String("step", "", "")
	_, err := prepareWizardCommand(command, []string{"ses-demo"}, rejectWizardChoice(t), func(string, string) (string, error) { return "Ready to deploy", nil }, rejectWizardPassword(t))
	note, _ := command.Flags().GetString("note")
	if err != nil || note != "Ready to deploy" {
		t.Fatalf("checkpoint = %s, %v", note, err)
	}
	if err := command.Flags().Set("step", "review"); err != nil {
		t.Fatal(err)
	}
	command.Flags().Lookup("note").Changed = false
	_, err = prepareWizardCommand(command, []string{"ses-demo"}, rejectWizardChoice(t), rejectWizardInput(t), rejectWizardPassword(t))
	if err != nil {
		t.Fatal(err)
	}
}
