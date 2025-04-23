package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/abdorrahmani/gophel/internal/monitor"
	"github.com/spf13/cobra"
)

var (
	monitorService monitor.MonitorService
)

func init() {
	monitorService = monitor.NewMonitorService()
}

var AuthCmd = &cobra.Command{
	Use:   "auth",
	Short: "Authenticates user with gophel.anophel.com",
	RunE: func(cmd *cobra.Command, args []string) error {
		var username, apiKey string
		fmt.Print("Enter username: ")
		fmt.Scanln(&username)
		fmt.Print("Enter API Key: ")
		fmt.Scanln(&apiKey)

		if err := authenticate(username, apiKey); err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}

		fmt.Println("Authentication successful!")

		// Start monitoring and send app information
		go func() {
			if err := startMonitoringAndSendApps(apiKey); err != nil {
				fmt.Printf("Error in monitoring: %v\n", err)
			}
		}()

		return nil
	},
}

func authenticate(username, apiKey string) error {
	client := &http.Client{}
	req, err := http.NewRequest("POST", "https://gophel.anophel.com/api/v1/gophel/auth", nil)
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("X-Username", username)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("invalid credentials: status code %d", resp.StatusCode)
	}

	return nil
}

func startMonitoringAndSendApps(apiKey string) error {
	// Get list of apps
	apps, err := app.GetGophelApps()
	if err != nil {
		return fmt.Errorf("error getting apps: %w", err)
	}

	// Send apps to server
	if err := sendAppsToServer(apps, apiKey); err != nil {
		return fmt.Errorf("error sending apps to server: %w", err)
	}

	// Start WebSocket monitoring
	if err := monitorService.StartMonitoring(apiKey); err != nil {
		return fmt.Errorf("error starting monitoring: %w", err)
	}

	return nil
}

func sendAppsToServer(apps []string, apiKey string) error {
	client := &http.Client{}

	body := struct {
		Apps []string `json:"apps"`
	}{
		Apps: apps,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("error marshaling apps: %w", err)
	}

	req, err := http.NewRequest("POST", "https://gophel.anophel.com/api/v1/gophel/apps", bytes.NewBuffer(jsonBody))
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned status code %d", resp.StatusCode)
	}

	return nil
}
