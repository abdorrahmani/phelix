package monitor

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/gorilla/websocket"
)

const (
	// Connection settings
	pingInterval         = 30 * time.Second
	metricsInterval      = 5 * time.Second
	reconnectDelay       = 5 * time.Second
	maxReconnectAttempts = 3

	// WebSocket message types
	messageTypePing = "ping"
	messageTypePong = "pong"
)

// MonitorService defines the interface for monitoring operations
type MonitorService interface {
	StartMonitoring(apiKey string) error
	StopMonitoring() error
	SendCommand(cmd Command) error
}

// Command represents a remote command to be executed
type Command struct {
	Type    string `json:"type"` // "start", "stop", "restart"
	AppName string `json:"appName"`
}

// AppMetrics represents the metrics collected for an application
type AppMetrics struct {
	AppName     string    `json:"appName"`
	CPUUsage    float64   `json:"cpuUsage"`
	MemoryUsage float64   `json:"memoryUsage"`
	Status      string    `json:"status"`
	Timestamp   time.Time `json:"timestamp"`
	PID         int       `json:"pid"`
	Uptime      string    `json:"uptime"`
	BuildStatus string    `json:"buildStatus"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// monitorService implements the MonitorService interface
type monitorService struct {
	conn              *websocket.Conn
	stopChan          chan struct{}
	mu                sync.Mutex
	apiKey            string
	reconnectAttempts int
}

// NewMonitorService creates a new instance of MonitorService
func NewMonitorService() MonitorService {
	return &monitorService{
		stopChan: make(chan struct{}),
	}
}

// StartMonitoring establishes a WebSocket connection and starts monitoring
func (m *monitorService) StartMonitoring(apiKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.conn != nil {
		return fmt.Errorf("monitoring already started")
	}

	// Get API key from session file if not provided
	if apiKey == "" {
		var err error
		apiKey, err = m.getAPIKeyFromSession()
		if err != nil {
			return fmt.Errorf("failed to get API key: %w", err)
		}
	}

	m.apiKey = apiKey
	if err := m.connect(); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	// Start goroutines for handling commands and sending metrics
	go m.handleCommands()
	go m.sendMetrics()
	go m.keepAlive()

	return nil
}

// getAPIKeyFromSession retrieves the API key from the session file
func (m *monitorService) getAPIKeyFromSession() (string, error) {
	sessionFile := filepath.Join(os.Getenv("HOME"), ".gophel", "session.json")
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return "", fmt.Errorf("error reading session file: %w", err)
	}

	var session struct {
		SessionID string    `json:"sessionID"`
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}

	if err := json.Unmarshal(data, &session); err != nil {
		return "", fmt.Errorf("error parsing session file: %w", err)
	}

	if time.Now().After(session.ExpiresAt) {
		return "", fmt.Errorf("session expired, please authenticate again")
	}

	return session.Token, nil
}

// connect establishes a WebSocket connection
func (m *monitorService) connect() error {
	wsURL := fmt.Sprintf("wss://gophel.anophel.com/api/v1/gophel/ws?apiKey=%s", m.apiKey)
	log.Printf("Attempting to connect to WebSocket at %s", wsURL)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to WebSocket: %w", err)
	}

	m.conn = conn
	log.Println("WebSocket connection established successfully")
	return nil
}

// keepAlive sends periodic ping messages to keep the connection alive
func (m *monitorService) keepAlive() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.mu.Lock()
			if m.conn != nil {
				if err := m.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					log.Printf("Error sending ping: %v", err)
					m.reconnect()
				}
			}
			m.mu.Unlock()
		}
	}
}

// reconnect attempts to reestablish the WebSocket connection
func (m *monitorService) reconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.reconnectAttempts >= maxReconnectAttempts {
		log.Printf("Max reconnection attempts reached (%d)", maxReconnectAttempts)
		return
	}

	if m.conn != nil {
		m.conn.Close()
		m.conn = nil
	}

	time.Sleep(reconnectDelay)
	if err := m.connect(); err != nil {
		log.Printf("Failed to reconnect: %v", err)
		m.reconnectAttempts++
		return
	}

	m.reconnectAttempts = 0
	log.Println("Successfully reconnected to WebSocket")
}

// StopMonitoring closes the WebSocket connection and stops monitoring
func (m *monitorService) StopMonitoring() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.conn == nil {
		return fmt.Errorf("monitoring not started")
	}

	close(m.stopChan)
	if err := m.conn.Close(); err != nil {
		return fmt.Errorf("failed to close WebSocket connection: %w", err)
	}

	m.conn = nil
	return nil
}

// SendCommand sends a command to the server
func (m *monitorService) SendCommand(cmd Command) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.conn == nil {
		return fmt.Errorf("WebSocket connection not established")
	}

	return m.conn.WriteJSON(cmd)
}

// handleCommands processes incoming commands from the server
func (m *monitorService) handleCommands() {
	for {
		select {
		case <-m.stopChan:
			return
		default:
			m.mu.Lock()
			if m.conn == nil {
				m.mu.Unlock()
				time.Sleep(time.Second)
				continue
			}

			var cmd Command
			if err := m.conn.ReadJSON(&cmd); err != nil {
				log.Printf("Error reading command: %v", err)
				m.mu.Unlock()
				m.reconnect()
				continue
			}
			m.mu.Unlock()

			if err := m.executeCommand(cmd); err != nil {
				log.Printf("Error executing command: %v", err)
			}
		}
	}
}

// executeCommand executes a received command
func (m *monitorService) executeCommand(cmd Command) error {
	apps := app.Manager.ListApplications()
	var targetAppID string
	for _, app := range apps {
		if app.Name == cmd.AppName {
			targetAppID = app.ID
			break
		}
	}

	if targetAppID == "" {
		return fmt.Errorf("app not found: %s", cmd.AppName)
	}

	var err error
	switch cmd.Type {
	case "start":
		err = app.Manager.StartApplication(targetAppID, 0, cmd.AppName)
	case "stop":
		err = app.Manager.StopApplication(targetAppID)
	case "restart":
		err = app.Manager.RestartApplication(targetAppID)
	default:
		return fmt.Errorf("unknown command type: %s", cmd.Type)
	}

	return err
}

// sendMetrics collects and sends metrics to the server
func (m *monitorService) sendMetrics() {
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			metrics := m.collectMetrics()
			m.mu.Lock()
			if m.conn != nil {
				if err := m.conn.WriteJSON(metrics); err != nil {
					log.Printf("Error sending metrics: %v", err)
					m.mu.Unlock()
					m.reconnect()
					continue
				}
			}
			m.mu.Unlock()
		}
	}
}

// collectMetrics collects metrics for all applications
func (m *monitorService) collectMetrics() []AppMetrics {
	var metrics []AppMetrics
	apps := app.Manager.ListApplications()

	for _, appInfo := range apps {
		status, err := app.Manager.StatusApplication(appInfo.ID)
		if err != nil {
			log.Printf("Error getting status for app %s: %v", appInfo.Name, err)
			continue
		}

		metrics = append(metrics, AppMetrics{
			AppName:     appInfo.Name,
			Status:      appInfo.Status,
			PID:         appInfo.PID,
			Uptime:      appInfo.Uptime,
			Timestamp:   time.Now(),
			CPUUsage:    status.CPUUsage,
			MemoryUsage: float64(status.RAMUsage) / (1024 * 1024), // Convert to MB
			BuildStatus: appInfo.BuildStatus,
			CreatedAt:   appInfo.CreatedAt,
			UpdatedAt:   appInfo.UpdatedAt,
		})
	}

	return metrics
}
