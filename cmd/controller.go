package cmd

import (
	"github.com/abdorrahmani/phelix/internal/controller"
	"github.com/spf13/cobra"
)

var ControllerCmd = &cobra.Command{
	Use:   "controller",
	Short: "Run the single-owner Phelix Controller",
	Args:  cobra.NoArgs,
	RunE:  runController,
}

func runController(command *cobra.Command, args []string) error {
	cfg, err := controller.LoadConfig()
	if err != nil {
		return err
	}
	owner, err := controller.New(command.Context(), cfg)
	if err != nil {
		return err
	}
	defer owner.Close()
	return runMonitor()
}
