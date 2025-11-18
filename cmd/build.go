package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/gophel/cmd/auth"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
)

const (
	defaultPort = 8080
)

var (
	buildPort int
)

var BuildCmd = &cobra.Command{
	Use:   "build <NAME> --port <PORT>",
	Short: "Builds and runs a Go application with a specified name",
	Long:  "Compiles a Go application from the current directory with the given name and starts it immediately",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateSession(); err != nil {
		}

		name := args[0]
		if err := validateName(name); err != nil {
			return err
		}

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("  ⚠ Failed to load state: %w", err)
		}

		if err := validateUniqueName(name); err != nil {
			return err
		}

		id := app.Manager.GenerateAppID()
		fmt.Printf("• Building application '%s', ID: %s\n", name, id)

		if err := createAppEntry(id, name); err != nil {
			return err
		}

		if err := buildApplication(id); err != nil {
			return err
		}

		if err := startApplication(id, name); err != nil {
			return err
		}

		if err := auth.SendAppsToServer(); err != nil {
			log.Printf("⚠ Error sending apps to server: %v", err)
			fmt.Printf("⚠ Warning: Failed to send app information to server: %v\n", err)
		}

		fmt.Printf("✓ Application '%s' (ID: %s) started successfully on port %d\n", name, id, buildPort)
		return nil
	},
}

func init() {
	BuildCmd.Flags().IntVarP(&buildPort, "port", "p", defaultPort, "Port to run the application on")
}

func validateSession() error {
	sessionFile := filepath.Join(os.Getenv("HOME"), ".gophel", "session.json")
	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		return fmt.Errorf("⚠ Authentication required. Please run 'gophel auth' first")
	}

	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return fmt.Errorf(" ⚠ error reading session file: %w", err)
	}

	var session struct {
		SessionID string    `json:"sessionID"`
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}

	if err := json.Unmarshal(data, &session); err != nil {
		return fmt.Errorf("⚠ error parsing session file: %w", err)
	}

	if time.Now().After(session.ExpiresAt) {
		return fmt.Errorf("⚠ session expired. Please run 'gophel auth' again")
	}

	return nil
}

func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("⚠ application name cannot be empty")
	}
	return nil
}

func validateUniqueName(name string) error {
	for _, app := range app.Manager.(*app.AppManager).Apps {
		if app.Name == name {
			return fmt.Errorf("⚠ application name '%s' is already in use", name)
		}
	}
	return nil
}

func createAppEntry(id, name string) error {
	if appManager, ok := app.Manager.(*app.AppManager); ok {
		now := time.Now()
		currentDir, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("⚠ failed to get current directory: %v", err)
		}
		appManager.Apps[id] = &app.AppInfo{
			ID:          id,
			Name:        name,
			Status:      "initializing",
			BuildStatus: "building",
			CreatedAt:   now,
			UpdatedAt:   now,
			Directory:   currentDir,
		}
		return appManager.SaveState()
	}
	return fmt.Errorf("⚠ invalid app manager type")
}

func FindMainFile(root string) (string, error) {
	var mainFile string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "main.go" {
			mainFile = path
			return io.EOF
		}
		return nil
	})
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("error walking directory: %w", err)
	}
	if mainFile == "" {
		return "", fmt.Errorf("⚠ no main.go file found in the directory")
	}
	return mainFile, nil
}

func buildApplication(id string) error {
	outputPath := filepath.Join(".", fmt.Sprintf("app_%s", id))
	projectRoot := app.Manager.(*app.AppManager).Apps[id].Directory

	mainFile, err := FindMainFile(projectRoot)
	if err != nil {
		return fmt.Errorf("⚠ build failed: %v", err)
	}

	realPath, _ := filepath.Rel(projectRoot, mainFile)

	cmd := exec.Command("go", "build", "-o", outputPath, realPath)
	cmd.Dir = projectRoot

	if output, err := cmd.CombinedOutput(); err != nil {
		if appManager, ok := app.Manager.(*app.AppManager); ok {
			if app, exists := appManager.Apps[id]; exists {
				app.BuildStatus = "failed"
				app.UpdatedAt = time.Now()
				appManager.SaveState()
			}
		}
		return fmt.Errorf("⚠ build failed: %v\nOutput: %s", err, string(output))
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

func startApplication(id, name string) error {
	if err := app.Manager.StartApplication(id, buildPort, name); err != nil {
		return fmt.Errorf("⚠ failed to start application: %w", err)
	}
	return nil
}
