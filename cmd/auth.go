package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/abdorrahmani/gophel/internal/monitor"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	monitorService monitor.MonitorService
	logFile        *os.File
)

func init() {
	monitorService = monitor.NewMonitorService()
	setupLogging()
}

func setupLogging() {
	logDir := filepath.Join(os.Getenv("HOME"), ".gophel", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		fmt.Printf("Error creating log directory: %v\n", err)
		return
	}

	logPath := filepath.Join(logDir, "gophel.log")
	var err error
	logFile, err = os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Error opening log file: %v\n", err)
		return
	}

	log.SetOutput(logFile)
}

var AuthCmd = &cobra.Command{
	Use:   "auth",
	Short: "Authenticates user with gophel.anophel.com",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Check if already authenticated
		sessionFile := filepath.Join(os.Getenv("HOME"), ".gophel", "session.json")
		if _, err := os.Stat(sessionFile); err == nil {
			// Read session file to check if it's valid
			data, err := os.ReadFile(sessionFile)
			if err == nil {
				var session struct {
					SessionID string    `json:"sessionID"`
					Token     string    `json:"token"`
					ExpiresAt time.Time `json:"expiresAt"`
				}
				if err := json.Unmarshal(data, &session); err == nil {
					if time.Now().Before(session.ExpiresAt) {
						// Verify session with server
						client := &http.Client{}
						req, err := http.NewRequest("GET", "https://gophel.anophel.com/api/v1/gophel/auth/status", nil)
						if err == nil {
							req.Header.Set("X-Session-ID", session.SessionID)
							req.Header.Set("Authorization", "Bearer "+session.Token)
							resp, err := client.Do(req)
							if err == nil && resp.StatusCode == http.StatusOK {
								fmt.Println("You are already authenticated. Use 'gophel auth logout' to logout first.")
								return nil
							}
						}
					}
				}
			}
		}

		var username, apiKey string
		fmt.Print("Enter username: ")
		fmt.Scanln(&username)
		fmt.Print("Enter API Key: ")
		fmt.Scanln(&apiKey)

		if err := authenticate(username, apiKey); err != nil {
			log.Printf("Authentication failed: %v", err)
			return fmt.Errorf("authentication failed: %w", err)
		}

		// Print welcome message
		green := color.New(color.FgGreen).SprintFunc()
		bold := color.New(color.Bold).SprintFunc()
		fmt.Printf("\nHi %s to Gophel!\n", bold(username))
		fmt.Printf("You can monitor your apps at %s\n", green("gophel.anophel.com"))
		fmt.Printf("%s\n\n", green("Authentication successful!"))
		log.Printf("Authentication successful!")

		// Start monitoring and send app information
		go func() {
			log.Printf("Starting monitoring and app registration process...")

			if err := startMonitoringAndSendApps(apiKey); err != nil {
				log.Printf("Error in monitoring and app registration: %v", err)
				fmt.Printf("Error in monitoring and app registration: %v\n", err)
				return
			}

			log.Printf("Monitoring and app registration completed successfully")
		}()

		return nil
	},
}

var AuthStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Shows authentication status",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Check if session file exists
		sessionFile := filepath.Join(os.Getenv("HOME"), ".gophel", "session.json")
		if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
			fmt.Println("No active session found. Please run 'gophel auth' first.")
			return nil
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
			fmt.Println("Session has expired. Please run 'gophel auth' again.")
			return nil
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
			fmt.Println("Session is invalid. Please run 'gophel auth' again.")
			return nil
		}

		var status struct {
			User      string `json:"user"`
			SessionID string `json:"sessionID"`
			ExpiresAt string `json:"expiresAt"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			return fmt.Errorf("error decoding response: %w", err)
		}

		green := color.New(color.FgGreen).SprintFunc()
		fmt.Printf("Authenticated as: %s\n", green(status.User))
		fmt.Printf("Session ID: %s\n", status.SessionID)
		fmt.Printf("Expires at: %s\n", status.ExpiresAt)
		return nil
	},
}

var AuthLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Logs out the current user",
	RunE: func(cmd *cobra.Command, args []string) error {
		sessionFile := filepath.Join(os.Getenv("HOME"), ".gophel", "session.json")
		if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
			fmt.Println("No active session found.")
			return nil
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

		// Call logout endpoint
		client := &http.Client{}
		req, err := http.NewRequest("POST", "https://gophel.anophel.com/api/v1/gophel/auth/logout", nil)
		if err != nil {
			return fmt.Errorf("error creating request: %w", err)
		}

		req.Header.Set("X-Session-ID", session.SessionID)
		req.Header.Set("Authorization", "Bearer "+session.Token)

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("error sending logout request: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("logout failed with status code %d", resp.StatusCode)
		}

		// Remove session file
		if err := os.Remove(sessionFile); err != nil {
			return fmt.Errorf("error removing session file: %w", err)
		}

		fmt.Println("Successfully logged out.")
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

	log.Printf("Attempting authentication for user: %s", username)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("invalid credentials: status code %d", resp.StatusCode)
	}

	var response struct {
		SessionID string `json:"sessionID"`
		Token     string `json:"token"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return fmt.Errorf("error decoding response: %w", err)
	}

	// Store the session information
	if err := storeSession(response.SessionID, response.Token); err != nil {
		return fmt.Errorf("error storing session: %w", err)
	}

	return nil
}

func storeSession(sessionID, token string) error {
	sessionDir := filepath.Join(os.Getenv("HOME"), ".gophel")
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return err
	}

	sessionFile := filepath.Join(sessionDir, "session.json")
	session := struct {
		SessionID string    `json:"sessionID"`
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}{
		SessionID: sessionID,
		Token:     token,
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}

	data, err := json.Marshal(session)
	if err != nil {
		return err
	}

	return os.WriteFile(sessionFile, data, 0600)
}

func startMonitoringAndSendApps(apiKey string) error {
	// Get list of apps
	apps, err := app.GetGophelApps()
	if err != nil {
		log.Printf("Error getting apps: %v", err)
		return fmt.Errorf("error getting apps: %w", err)
	}

	log.Printf("Found %d apps to register", len(apps))

	// Send apps to server
	if err := sendAppsToServer(); err != nil {
		log.Printf("Error sending apps to server: %v", err)
		return fmt.Errorf("error sending apps to server: %w", err)
	}

	log.Printf("Successfully registered %d apps", len(apps))

	// Start WebSocket monitoring
	if err := monitorService.StartMonitoring(apiKey); err != nil {
		log.Printf("Error starting WebSocket monitoring: %v", err)
		return fmt.Errorf("error starting monitoring: %w", err)
	}

	log.Println("WebSocket monitoring started successfully")
	return nil
}

func sendAppsToServer() error {
	sessionFile := filepath.Join(os.Getenv("HOME"), ".gophel", "session.json")
	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		return fmt.Errorf("authentication required. Please run 'gophel auth' first")
	}

	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return fmt.Errorf("error reading session file:%w", err)
	}

	var session struct {
		SessionID string    `json:"sessionID"`
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}

	if err := json.Unmarshal(data, &session); err != nil {
		return fmt.Errorf("error parsing session file: %w", err)
	}

	client := &http.Client{}

	// Get all app details
	appList := app.Manager.ListApplications()
	var appDetails []struct {
		ID          uint      `json:"id"`
		Name        string    `json:"name"`
		Status      string    `json:"status"`
		PID         int       `json:"pid"`
		Uptime      string    `json:"uptime"`
		BuildStatus string    `json:"buildStatus"`
		CreatedAt   time.Time `json:"createdAt"`
		UpdatedAt   time.Time `json:"updatedAt"`
	}

	for _, app := range appList {
		u64, _ := strconv.ParseUint(app.ID, 10, 32)
		id := uint(u64)
		appDetails = append(appDetails, struct {
			ID          uint      `json:"id"`
			Name        string    `json:"name"`
			Status      string    `json:"status"`
			PID         int       `json:"pid"`
			Uptime      string    `json:"uptime"`
			BuildStatus string    `json:"buildStatus"`
			CreatedAt   time.Time `json:"createdAt"`
			UpdatedAt   time.Time `json:"updatedAt"`
		}{
			ID:          id,
			Name:        app.Name,
			Status:      app.Status,
			PID:         app.PID,
			Uptime:      app.Uptime,
			BuildStatus: app.BuildStatus,
			CreatedAt:   app.CreatedAt,
			UpdatedAt:   app.UpdatedAt,
		})
	}

	body := struct {
		Apps []struct {
			ID          uint      `json:"id"`
			Name        string    `json:"name"`
			Status      string    `json:"status"`
			PID         int       `json:"pid"`
			Uptime      string    `json:"uptime"`
			BuildStatus string    `json:"buildStatus"`
			CreatedAt   time.Time `json:"createdAt"`
			UpdatedAt   time.Time `json:"updatedAt"`
		} `json:"apps"`
	}{
		Apps: appDetails,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		log.Printf("error marshaling apps %s", err.Error())
		return fmt.Errorf("error marshaling apps: %w", err)
	}

	req, err := http.NewRequest("POST", "https://gophel.anophel.com/api/v1/gophel/apps", bytes.NewBuffer(jsonBody))
	if err != nil {
		log.Printf("Failed to send apps to server: %s", err.Error())
		return fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("Authorization", "Bearer "+session.Token)
	req.Header.Set("Content-Type", "application/json")

	log.Printf("Sending apps to server: %s", string(jsonBody))
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Failed to send apps to server: %s", err.Error())
		return fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		errMsg := string(bodyBytes)
		log.Printf("server returned status code %d: %s", resp.StatusCode, errMsg)
		return fmt.Errorf("server returned status code %d: %s", resp.StatusCode, errMsg)
	}

	log.Printf("apps sent to server successfully")
	return nil
}

func init() {
	AuthCmd.AddCommand(AuthStatusCmd)
	AuthCmd.AddCommand(AuthLogoutCmd)
}
