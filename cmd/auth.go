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

var AuthCmd = &cobra.Command{
	Use:   "auth",
	Short: "Authenticates user in gophel.anophel.com",
	Run: func(cmd *cobra.Command, args []string) {
		var username, apiKey string
		fmt.Print("Enter username: ")
		fmt.Scanln(&username)
		fmt.Print("Enter API Key: ")
		fmt.Scanln(&apiKey)

		// Create HTTP client with API key
		client := &http.Client{}
		req, err := http.NewRequest("POST", "https://gophel.anophel.com/api/v1/gophel/auth", nil)
		if err != nil {
			fmt.Println("Error creating request:", err)
			return
		}
		req.Header.Set("X-API-Key", apiKey)
		req.Header.Set("X-Username", username)

		resp, err := client.Do(req)
		if err != nil {
			fmt.Println("Authentication failed:", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			fmt.Println("Authentication failed: Invalid credentials")
			return
		}

		fmt.Println("Authentication successful!")

		// Start monitoring and send app information
		go func() {
			// Get list of apps
			apps, err := app.GetGophelApps()
			if err != nil {
				fmt.Printf("Error getting apps: %v\n", err)
				return
			}

			// Send apps to server
			if err := sendAppsToServer(apps, apiKey); err != nil {
				fmt.Printf("Error sending apps to server: %v\n", err)
				return
			}

			// Start WebSocket monitoring
			monitor.StartMonitoring(apiKey)
		}()
	},
}

func sendAppsToServer(apps []string, apiKey string) error {
	client := &http.Client{}

	// Prepare the request body
	body := struct {
		Apps []string `json:"apps"`
	}{
		Apps: apps,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", "https://gophel.anophel.com/api/v1/gophel/apps", bytes.NewBuffer(jsonBody))
	if err != nil {
		return err
	}

	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to send apps to server: status code %d", resp.StatusCode)
	}

	return nil
}
