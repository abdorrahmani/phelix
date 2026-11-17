package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
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
	case "build", "rebuild", "deploy", "rollback", "start", "stop", "restart", "remove", "status", "list", "log", "watch", "scale", "autoscale", "job", "hooks", "env", "secrets", "health", "proxy", "matrix", "operation", "inspect", "plan", "authz", "context", "session", "security", "webhook", "build-report", "deployment-report", "config":
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
				return phelixerr.New(phelixerr.Code(record.Error.Code), record.Error.Message)
			}
			return phelixerr.New(phelixerr.CodeProcessFailed, "Controller operation failed")
		}
	}
}
