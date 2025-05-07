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

const (
	sessionFile = "session.json"
	logFile     = "gophel.log"
)

var (
	monitorService monitor.MonitorService
	logFileHandle  *os.File
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

	logPath := filepath.Join(logDir, logFile)
	var err error
	logFileHandle, err = os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Error opening log file: %v\n", err)
		return
	}

	log.SetOutput(logFileHandle)
}

var AuthCmd = &cobra.Command{
	Use:   "auth",
	Short: "Authenticates user with gophel.anophel.com",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkExistingSession(); err != nil {
			return err
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

		printWelcomeMessage(username)
		return nil
	},
}

func checkExistingSession() error {
	sessionFile := filepath.Join(os.Getenv("HOME"), ".gophel", sessionFile)
	if _, err := os.Stat(sessionFile); err == nil {
		session, err := readSessionFile(sessionFile)
		if err == nil && time.Now().Before(session.ExpiresAt) {
			if err := verifySession(session); err == nil {
				fmt.Println("You are already authenticated. Use 'gophel auth logout' to logout first.")
				return nil
			}
		}
	}
	return nil
}

func readSessionFile(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading session file: %w", err)
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("error parsing session file: %w", err)
	}

	return &session, nil
}

func verifySession(session *Session) error {
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
		return fmt.Errorf("invalid session status: %d", resp.StatusCode)
	}

	return nil
}

func printWelcomeMessage(username string) {
	green := color.New(color.FgGreen).SprintFunc()
	bold := color.New(color.Bold).SprintFunc()
	fmt.Printf("\nHi %s to Gophel!\n", bold(username))
	fmt.Printf("You can monitor your apps at %s\n", green("gophel.anophel.com"))
	fmt.Printf("%s\n\n", green("Authentication successful!"))
	log.Printf("Authentication successful!")
}

var AuthStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Shows authentication status",
	RunE: func(cmd *cobra.Command, args []string) error {
		session, err := GetValidSession()
		if err != nil {
			return err
		}

		status, err := getSessionStatus(session)
		if err != nil {
			return err
		}

		printSessionStatus(status)
		return nil
	},
}

// GetValidSession returns a valid session if one exists
func GetValidSession() (*Session, error) {
	sessionFile := filepath.Join(os.Getenv("HOME"), ".gophel", sessionFile)
	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		return nil, fmt.Errorf("no active session found. Please run 'gophel auth' first")
	}

	session, err := readSessionFile(sessionFile)
	if err != nil {
		return nil, err
	}

	if time.Now().After(session.ExpiresAt) {
		return nil, fmt.Errorf("session has expired. Please run 'gophel auth' again")
	}

	return session, nil
}

func getSessionStatus(session *Session) (*SessionStatus, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", "https://gophel.anophel.com/api/v1/gophel/auth/status", nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("Authorization", "Bearer "+session.Token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error checking session status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("session is invalid. Please run 'gophel auth' again")
	}

	var status SessionStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("error decoding response: %w", err)
	}

	return &status, nil
}

func printSessionStatus(status *SessionStatus) {
	green := color.New(color.FgGreen).SprintFunc()
	fmt.Printf("Authenticated as: %s\n", green(status.User))
	fmt.Printf("Session ID: %s\n", status.SessionID)
	fmt.Printf("Expires at: %s\n", status.ExpiresAt)
}

var AuthLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Logs out the current user",
	RunE: func(cmd *cobra.Command, args []string) error {
		session, err := GetValidSession()
		if err != nil {
			fmt.Println("No active session found.")
			return nil
		}

		if err := performLogout(session); err != nil {
			return err
		}

		if err := removeSessionFile(); err != nil {
			return err
		}

		fmt.Println("Successfully logged out.")
		return nil
	},
}

func performLogout(session *Session) error {
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

	return nil
}

func removeSessionFile() error {
	sessionFile := filepath.Join(os.Getenv("HOME"), ".gophel", sessionFile)
	if err := os.Remove(sessionFile); err != nil {
		return fmt.Errorf("error removing session file: %w", err)
	}
	return nil
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

	return storeSession(response.SessionID, response.Token)
}

func storeSession(sessionID, token string) error {
	sessionDir := filepath.Join(os.Getenv("HOME"), ".gophel")
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return err
	}

	sessionFile := filepath.Join(sessionDir, sessionFile)
	session := Session{
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

func sendAppsToServer() error {
	session, err := GetValidSession()
	if err != nil {
		log.Printf("[Monitor] Authentication required: %v", err)
		return fmt.Errorf("authentication required. Please run 'gophel auth' first")
	}

	appList := app.Manager.ListApplications()
	log.Printf("[Monitor] Preparing to send %d apps to server", len(appList))

	var appDetails []AppDetail

	for _, app := range appList {
		u64, _ := strconv.ParseUint(app.ID, 10, 32)
		id := uint(u64)
		appDetails = append(appDetails, AppDetail{
			ID:          id,
			Name:        app.Name,
			Status:      app.Status,
			PID:         app.PID,
			Uptime:      app.Uptime,
			BuildStatus: app.BuildStatus,
			CreatedAt:   app.CreatedAt,
			UpdatedAt:   app.UpdatedAt,
		})
		log.Printf("[Monitor] Prepared app details for %s (ID: %d)", app.Name, id)
	}

	body := struct {
		Apps []AppDetail `json:"apps"`
	}{
		Apps: appDetails,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		log.Printf("[Monitor] Error marshaling apps: %v", err)
		return fmt.Errorf("error marshaling apps: %w", err)
	}

	client := &http.Client{}
	req, err := http.NewRequest("POST", "https://gophel.anophel.com/api/v1/gophel/apps", bytes.NewBuffer(jsonBody))
	if err != nil {
		log.Printf("[Monitor] Error creating request: %v", err)
		return fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("Authorization", "Bearer "+session.Token)
	req.Header.Set("Content-Type", "application/json")

	log.Printf("[Monitor] Sending apps to server...")
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[Monitor] Error sending request: %v", err)
		return fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		log.Printf("[Monitor] Server returned error status %d: %s", resp.StatusCode, string(bodyBytes))
		return fmt.Errorf("server returned status code %d: %s", resp.StatusCode, string(bodyBytes))
	}

	log.Printf("[Monitor] Successfully sent apps to server")
	return nil
}

// Session represents the user's session information
type Session struct {
	SessionID string    `json:"sessionID"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// SessionStatus represents the current session status
type SessionStatus struct {
	User      string `json:"user"`
	SessionID string `json:"sessionID"`
	ExpiresAt string `json:"expiresAt"`
}

// AppDetail represents the details of an application
type AppDetail struct {
	ID          uint      `json:"id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	PID         int       `json:"pid"`
	Uptime      string    `json:"uptime"`
	BuildStatus string    `json:"buildStatus"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func init() {
	AuthCmd.AddCommand(AuthStatusCmd)
	AuthCmd.AddCommand(AuthLogoutCmd)
}
