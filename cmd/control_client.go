package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/controlapi"
	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	"github.com/abdorrahmani/phelix/internal/controller"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func managedCommand(command *cobra.Command) bool {
	for command.Parent() != nil && command.Parent().Parent() != nil {
		command = command.Parent()
	}
	switch command.Name() {
	case "build", "dockerize", "rebuild", "deploy", "rollback", "start", "stop", "restart", "remove", "status", "list", "log", "watch", "scale", "autoscale", "job", "hooks", "env", "secrets", "health", "proxy", "matrix", "operation", "inspect", "plan", "authz", "context", "session", "security", "webhook", "build-report", "deployment-report", "config":
		return true
	}
	return false
}

func RouteControl(root *cobra.Command) {
	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		for _, child := range command.Commands() {
			visit(child)
		}
		if !managedCommand(command) || command.RunE == nil {
			return
		}
		command.RunE = runControlClient
	}
	visit(root)
}

func controlArguments(command *cobra.Command, args []string) []string {
	path := strings.Fields(command.CommandPath())
	argv := append([]string{}, path[1:]...)
	argv = append(argv, args...)
	command.Flags().Visit(func(flag *pflag.Flag) {
		if flag.Name == "help" {
			return
		}
		if value, ok := flag.Value.(pflag.SliceValue); ok {
			for _, entry := range value.GetSlice() {
				argv = append(argv, "--"+flag.Name+"="+entry)
			}
		} else {
			argv = append(argv, "--"+flag.Name+"="+flag.Value.String())
		}
	})
	return argv
}

func runControlClient(command *cobra.Command, args []string) error {
	cfg, err := controller.LoadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(command.Context(), 3*time.Second)
	conn, client, err := controlapi.Connect(ctx, cfg.Socket)
	cancel()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConnection, "Controller unavailable", err)
	}
	defer conn.Close()
	if IsInteractive() {
		args, err = selectControlApp(command, args, client, PromptSelect)
		if err != nil {
			return err
		}
	}
	directory, err := os.Getwd()
	if err != nil {
		return err
	}
	var input []byte
	if command.Name() == "set" && strings.Contains(command.CommandPath(), "secrets") {
		input, err = io.ReadAll(io.LimitReader(command.InOrStdin(), (1<<20)+1))
		if err != nil {
			return err
		}
		if len(input) > 1<<20 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "secret input exceeds 1 MiB")
		}
	}
	submitCtx, submitCancel := context.WithTimeout(command.Context(), 5*time.Second)
	operation, err := client.Execute(submitCtx, &pb.CommandRequest{Argv: controlArguments(command, args), Directory: directory, Input: input})
	submitCancel()
	if err != nil {
		return err
	}
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	stream, err := client.WatchOperation(watchCtx, &pb.WatchRequest{Id: operation.Id})
	if err != nil {
		return err
	}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-command.Context().Done():
			cancelCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			if operation.Cancellable {
				client.CancelOperation(cancelCtx, &pb.OperationRequest{Id: operation.Id})
			}
			watchCancel()
		case <-finished:
		}
	}()
	for {
		event, err := stream.Recv()
		if err != nil {
			if command.Context().Err() != nil {
				return command.Context().Err()
			}
			return fmt.Errorf("operation %s stream ended; inspect with 'phelix controller operation %s': %w", operation.Id, operation.Id, err)
		}
		writer := command.OutOrStdout()
		if event.Stderr {
			writer = command.ErrOrStderr()
		}
		if event.Output != "" {
			if _, err := io.WriteString(writer, event.Output); err != nil {
				return err
			}
		}
		record := event.Operation
		if record == nil {
			continue
		}
		switch record.State {
		case pb.OperationState_SUCCEEDED:
			return nil
		case pb.OperationState_FAILED, pb.OperationState_CANCELLED, pb.OperationState_INTERRUPTED:
			if record.Error != nil {
				return &controlOperationError{record}
			}
			return phelixerr.New(phelixerr.CodeProcessFailed, "Controller operation failed")
		}
	}
}

func selectControlApp(command *cobra.Command, args []string, client pb.ControlClient, choose func(string, []string) (string, error)) ([]string, error) {
	if len(args) > 0 || command.Parent() != command.Root() || (command.Name() != "status" && command.Name() != "remove") {
		return args, nil
	}
	if workspace, _ := command.Flags().GetBool("workspace"); workspace {
		return args, nil
	}
	ctx, cancel := context.WithTimeout(command.Context(), 10*time.Second)
	defer cancel()
	response, err := client.Query(ctx, &pb.QueryRequest{Kind: pb.QueryKind_APPLICATIONS})
	if err != nil {
		return nil, err
	}
	if len(response.Applications) == 0 {
		return nil, phelixerr.New(phelixerr.CodeNotFound, "no applications found; build one first with 'phelix build <NAME>'")
	}
	sort.Slice(response.Applications, func(i, j int) bool {
		a, b := response.Applications[i], response.Applications[j]
		if a.Name == b.Name {
			return a.Id < b.Id
		}
		return a.Name < b.Name
	})
	options := make([]string, 0, len(response.Applications))
	for _, application := range response.Applications {
		label := application.Id
		if application.Name != "" {
			label = fmt.Sprintf("%s (%s)", application.Name, application.Id)
		}
		options = append(options, label)
	}
	message := "Select application to inspect"
	if command.Name() == "remove" {
		message = "Select application to remove"
	}
	selected, err := choose(message, options)
	if err != nil {
		return nil, err
	}
	for i, option := range options {
		if option == selected {
			return []string{response.Applications[i].Id}, nil
		}
	}
	return nil, phelixerr.New(phelixerr.CodeInvalidArgument, "selected application was not found")
}

type controlOperationError struct{ result *pb.Operation }

func (e *controlOperationError) Error() string { return e.result.Error.Message }
func (e *controlOperationError) Unwrap() error {
	return phelixerr.New(phelixerr.Code(e.result.Error.Code), e.result.Error.Message)
}
func (e *controlOperationError) RenderedError() (string, int) {
	return e.result.Error.Display, int(e.result.ExitCode)
}
