package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/abdorrahmani/gophel/cmd"
	"github.com/abdorrahmani/gophel/internal/monitor"
	"github.com/spf13/cobra"
)

var (
	monitorService monitor.MonitorService
)

func main() {
	if err := checkGoInstallation(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// Initialize monitor service
	monitorService = monitor.NewMonitorService()

	rootCmd := &cobra.Command{
		Use:     "gophel",
		Short:   "Gophel - Go Application Manager",
		Version: cmd.Version,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Only check authentication for auth status command
			if cmd.Name() == "auth" && len(args) > 0 && args[0] == "status" {
				if err := checkAuthStatus(); err != nil {
					fmt.Printf("Authentication required: %v\n", err)
					fmt.Println("Please run 'gophel auth' to authenticate first")
					os.Exit(1)
				}
			}
		},
	}

	// Add commands
	rootCmd.AddCommand(cmd.BuildCmd)
	rootCmd.AddCommand(cmd.RebuildCmd)
	rootCmd.AddCommand(cmd.StartCmd)
	rootCmd.AddCommand(cmd.RestartCmd)
	rootCmd.AddCommand(cmd.StatusCmd)
	rootCmd.AddCommand(cmd.StopCmd)
	rootCmd.AddCommand(cmd.ListCmd)
	rootCmd.AddCommand(cmd.LogCmd)
	rootCmd.AddCommand(cmd.AuthCmd)
	rootCmd.AddCommand(cmd.VersionCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func checkAuthStatus() error {
	// Check if session file exists
	sessionFile := filepath.Join(os.Getenv("HOME"), ".gophel", "session.json")
	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		return fmt.Errorf("no active session found")
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
		return fmt.Errorf("session expired")
	}

	// Verify session with server
	client := &http.Client{}
	req, err := http.NewRequest("GET", "https://gophel.anophel.com/api/v1/gophel/auth/status", nil)
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("Authorization", "Bearer "+session.Token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("error checking session status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("invalid session")
	}

	return nil
}

func checkGoInstallation() error {
	if _, err := exec.LookPath("go"); err != nil {
		fmt.Println("Go not found. Attempting to install the latest version...")
		if runtime.GOOS != "linux" {
			return fmt.Errorf("automatic Go installation is only supported on Linux; please install Go manually")
		}

		cmd := exec.Command("sudo", "apt", "update")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to run apt update: %v", err)
		}
		cmd = exec.Command("sudo", "apt", "install", "-y", "golang-go")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to install Go: %v", err)
		}
		fmt.Println("Go installed successfully!")
	}
	return nil
}
