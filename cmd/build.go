package cmd

import (
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
)

var BuildCmd = &cobra.Command{
	Use:   "build <NAME> --port <PORT>",
	Short: "Builds and runs a Go application with a specified name",
	Long:  "Compiles a Go application from the current directory with the given name and starts it immediately",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if err := validateName(name); err != nil {
			return err
		}

		id := app.Manager.GenerateAppID()
		fmt.Printf("Building application '%s', ID: %s\n", name, id)

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("failed to load state: %v", err)
		}

		if err := validateUniqueName(name); err != nil {
			return err
		}

		if err := buildApplication(id); err != nil {
			return err
		}

		if err := app.Manager.StartApplication(id, port, name); err != nil {
			return fmt.Errorf("failed to start application: %v", err)
		}

		fmt.Printf("Application '%s' (ID: %s) started successfully on port %d\n", name, id, port)
		return nil
	},
}

func init() {
	BuildCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to run the application on")
}

func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("application name cannot be empty")
	}
	return nil
}

func validateUniqueName(name string) error {
	for _, app := range app.Manager.(*app.AppManager).Apps {
		if app.Name == name {
			return fmt.Errorf("application name '%s' is already in use", name)
		}
	}
	return nil
}

func buildApplication(id string) error {
	outputPath := filepath.Join(".", fmt.Sprintf("app_%s", id))
	cmd := exec.Command("go", "build", "-o", outputPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build failed: %v\nOutput: %s", err, string(output))
	}
	return nil
}
