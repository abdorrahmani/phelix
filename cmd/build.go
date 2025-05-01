package cmd

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
)

var BuildCmd = &cobra.Command{
	Use:   "build <NAME> --port <PORT>",
	Short: "Builds and runs a Go application with a specified name",
	Long:  "Compiles a Go application from the current directory with the given name and starts it immediately",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Check authentication
		sessionFile := filepath.Join(os.Getenv("HOME"), ".gophel", "session.json")
		if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
			return fmt.Errorf("authentication required. Please run 'gophel auth' first")
		}

		// Read session file
		data, err := os.ReadFile(sessionFile)
		if err != nil {
			return fmt.Errorf("error reading session file: %w", err)
		}

		var session struct {
			SessionID string    `json:"sessionID"`
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expiresAt"`
		}

		if err := json.Unmarshal(data, &session); err != nil {
			return fmt.Errorf("error parsing session file: %w", err)
		}

		// Check if session is expired
		if time.Now().After(session.ExpiresAt) {
			return fmt.Errorf("session expired. Please run 'gophel auth' again")
		}

		name := args[0]
		if err := validateName(name); err != nil {
			return err
		}

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("failed to load state: %v", err)
		}

		if err := validateUniqueName(name); err != nil {
			return err
		}

		id := app.Manager.GenerateAppID()
		fmt.Printf("Building application '%s', ID: %s\n", name, id)

		// Create initial app entry with timestamps
		if appManager, ok := app.Manager.(*app.AppManager); ok {
			now := time.Now()
			appManager.Apps[id] = &app.AppInfo{
				ID:          id,
				Name:        name,
				Status:      "initializing",
				BuildStatus: "building",
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			appManager.SaveState()
		}

		if err := buildApplication(id); err != nil {
			return err
		}

		if err := app.Manager.StartApplication(id, port, name); err != nil {
			return fmt.Errorf("failed to start application: %v", err)
		}

		// Send apps to server
		if err := sendAppsToServer(); err != nil {
			log.Printf("Error sending apps to server: %v", err)
			fmt.Printf("Warning: Failed to send app information to server: %v\n", err)
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
		if appManager, ok := app.Manager.(*app.AppManager); ok {
			if app, exists := appManager.Apps[id]; exists {
				app.BuildStatus = "failed"
				app.UpdatedAt = time.Now()
				appManager.SaveState()
			}
		}
		return fmt.Errorf("build failed: %v\nOutput: %s", err, string(output))
	}

	if appManager, ok := app.Manager.(*app.AppManager); ok {
		if app, exists := appManager.Apps[id]; exists {
			app.BuildStatus = "success"
			app.UpdatedAt = time.Now()
			appManager.SaveState()
		}
	}
	return nil
}
