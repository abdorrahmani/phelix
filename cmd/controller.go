package cmd

import (
	"context"
	"fmt"
	"github.com/abdorrahmani/phelix/internal/controlapi"
	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	"github.com/abdorrahmani/phelix/internal/controller"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"os"
	"time"
)

var ControllerCmd = &cobra.Command{
	Use:   "controller",
	Short: "Run the single-owner Phelix Controller",
	Args:  cobra.NoArgs,
	RunE:  runController,
}

func runController(command *cobra.Command, args []string) error {
	if err := os.Setenv("PHELIX_DATA_DIR", server.DataDir()); err != nil {
		return err
	}
	cfg, err := controller.LoadConfig()
	if err != nil {
		return err
	}
	owner, err := controller.New(command.Context(), cfg)
	if err != nil {
		return err
	}
	defer owner.Close()
	os.Setenv("PHELIX_CONTROLLER_MODE", cfg.Mode)
	os.Setenv("PHELIX_CONTROLLER_NETWORK", cfg.Network)
	os.Setenv("PHELIX_CONTROLLER_ID", owner.Identity)
	return runMonitorContext(command.Context(), func() (func(), error) { return startControlService(command, owner) })
}

func init() {
	for _, name := range []string{"info", "health", "operation"} {
		sub := &cobra.Command{Use: name, Args: cobra.NoArgs, RunE: controllerQuery}
		if name == "operation" {
			sub.Use = "operation <id>"
			sub.Args = cobra.ExactArgs(1)
			sub.Flags().Bool("cancel", false, "Request safe operation cancellation")
		}
		ControllerCmd.AddCommand(sub)
	}
}

func controllerQuery(command *cobra.Command, args []string) error {
	cfg, err := controller.LoadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(command.Context(), 5*time.Second)
	defer cancel()
	conn, client, err := controlapi.Connect(ctx, cfg.Socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	var raw []byte
	switch command.Name() {
	case "info":
		response, err := client.Info(ctx, &pb.Empty{})
		if err != nil {
			return err
		}
		raw, err = protojson.Marshal(response)
	case "health":
		response, err := client.Health(ctx, &pb.Empty{})
		if err != nil {
			return err
		}
		raw, err = protojson.Marshal(response)
	case "operation":
		response, err := client.GetOperation(ctx, &pb.OperationRequest{Id: args[0]})
		if requested, _ := command.Flags().GetBool("cancel"); requested {
			response, err = client.CancelOperation(ctx, &pb.OperationRequest{Id: args[0]})
		}
		if err != nil {
			return err
		}
		raw, err = protojson.Marshal(response)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(command.OutOrStdout(), string(raw))
	return err
}
