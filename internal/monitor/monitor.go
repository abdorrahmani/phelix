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
	metricsInterval      = 2 * time.Second
	reconnectDelay       = 5 * time.Second
	maxReconnectAttempts = 3
)

// MonitorService defines the interface for monitoring operations
type MonitorService interface {
	StartMonitoring() error
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
	AppID       string  `json:"appID"`
	CPUUsage    float64 `json:"cpuUsage"`
	MemoryUsage uint64  `json:"memoryUsage"`
}

// AppDetails represents the application details
type AppDetails struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Port        int       `json:"port"`
	BuildStatus string    `json:"buildStatus"`
	PID         int       `json:"pid"`
	Uptime      string    `json:"uptime"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Session represents the user's session information
type Session struct {
	SessionID string    `json:"sessionID"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// monitorService implements the MonitorService interface
type monitorService struct {
	conn              *websocket.Conn
	stopChan          chan struct{}
	mu                sync.Mutex
	session           Session
	reconnectAttempts int
}

// NewMonitorService creates a new instance of MonitorService
func NewMonitorService() MonitorService {
	return &monitorService{
		stopChan: make(chan struct{}),
	}
}

// StartMonitoring establishes a WebSocket connection and starts monitoring
func (m *monitorService) StartMonitoring() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.conn != nil {
		return fmt.Errorf("monitoring already started")
	}

	// Get session from file
	sessionFile := filepath.Join(os.Getenv("HOME"), ".gophel", "session.json")
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return fmt.Errorf("failed to read session file: %w", err)
	}

	if err := json.Unmarshal(data, &m.session); err != nil {
		return fmt.Errorf("failed to parse session file: %w", err)
	}

	if time.Now().After(m.session.ExpiresAt) {
		return fmt.Errorf("session expired, please authenticate again")
	}

	log.Printf("[WebSocket] Starting monitoring with session ID: %s", m.session.SessionID)
	if err := m.connect(); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	// Start goroutines for handling commands and sending metrics
	go m.handleCommands()
	go m.sendMetrics()
	go m.keepAlive()

	// Reset reconnect attempts since we've successfully connected
	m.reconnectAttempts = 0

	return nil
}

// connect establishes a WebSocket connection
func (m *monitorService) connect() error {
	wsURL := fmt.Sprintf("wss://gophel.anophel.com/api/v1/gophel/ws?token=%s&sessionID=%s", m.session.Token, m.session.SessionID)
	log.Printf("[WebSocket] Attempting to connect to %s", wsURL)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		log.Printf("[WebSocket] Connection failed: %v", err)
		return fmt.Errorf("failed to connect to WebSocket: %w", err)
	}

	m.conn = conn
	log.Printf("[WebSocket] Connection established successfully")
	return nil
}

// keepAlive sends periodic ping messages to keep the connection alive
func (m *monitorService) keepAlive() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	log.Printf("[WebSocket] Starting keep-alive with interval: %v", pingInterval)

	for {
		select {
		case <-m.stopChan:
			log.Printf("[WebSocket] Stopping keep-alive")
			return
		case <-ticker.C:
			m.mu.Lock()
			if m.conn != nil {
				log.Printf("[WebSocket] Sending ping")
				if err := m.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					log.Printf("[WebSocket] Error sending ping: %v", err)
					m.mu.Unlock()
					m.reconnect()
					continue
				}
				log.Printf("[WebSocket] Ping sent successfully")
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
		log.Printf("[WebSocket] Max reconnection attempts reached (%d), will retry after delay", maxReconnectAttempts)
		time.Sleep(reconnectDelay)
		m.reconnectAttempts = 0
	}

	log.Printf("[WebSocket] Attempting to reconnect (attempt %d/%d)", m.reconnectAttempts+1, maxReconnectAttempts)

	if m.conn != nil {
		m.conn.Close()
		m.conn = nil
	}

	time.Sleep(reconnectDelay)
	if err := m.connect(); err != nil {
		log.Printf("[WebSocket] Failed to reconnect: %v", err)
		m.reconnectAttempts++
		return
	}

	m.reconnectAttempts = 0
	log.Printf("[WebSocket] Successfully reconnected")
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
			AppID:       appInfo.ID,
			CPUUsage:    status.CPUUsage,
			MemoryUsage: status.RAMUsage,
		})
	}

	return metrics
}

// collectApps collects application details
func (m *monitorService) collectApps() []AppDetails {
	var apps []AppDetails
	appList := app.Manager.ListApplications()

	for _, appInfo := range appList {
		apps = append(apps, AppDetails{
			ID:          appInfo.ID,
			Name:        appInfo.Name,
			Status:      appInfo.Status,
			BuildStatus: appInfo.BuildStatus,
			PID:         appInfo.PID,
			Port:        appInfo.Port,
			Uptime:      appInfo.Uptime,
			CreatedAt:   appInfo.CreatedAt,
			UpdatedAt:   appInfo.UpdatedAt,
		})
	}
	return apps
}

// sendMetrics collects and sends metrics to the server
func (m *monitorService) sendMetrics() {
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()

	log.Printf("[WebSocket] Starting metrics sender with interval: %v", metricsInterval)

	for {
		select {
		case <-m.stopChan:
			log.Printf("[WebSocket] Stopping metrics sender")
			return
		case <-ticker.C:
			// Send metrics
			metrics := m.collectMetrics()
			log.Printf("[WebSocket] Collected metrics for %d apps", len(metrics))

			for _, metric := range metrics {
				message := map[string]interface{}{
					"type":    "metrics",
					"payload": metric,
				}

				m.mu.Lock()
				if m.conn != nil {
					log.Printf("[WebSocket] Sending metrics for app %s", metric.AppID)
					if err := m.conn.WriteJSON(message); err != nil {
						log.Printf("[WebSocket] Error sending metrics: %v", err)
						m.mu.Unlock()
						m.reconnect()
						continue
					}
					log.Printf("[WebSocket] Successfully sent metrics for app %s", metric.AppID)
				} else {
					log.Printf("[WebSocket] Connection is nil, attempting to reconnect")
					m.mu.Unlock()
					m.reconnect()
					continue
				}
				m.mu.Unlock()
			}

			// Send apps
			apps := m.collectApps()
			log.Printf("[WebSocket] Collected details for %d apps", len(apps))

			for _, app := range apps {
				message := map[string]interface{}{
					"type":    "apps",
					"payload": app,
				}

				m.mu.Lock()
				if m.conn != nil {
					log.Printf("[WebSocket] Sending app details for %s", app.Name)
					if err := m.conn.WriteJSON(message); err != nil {
						log.Printf("[WebSocket] Error sending app details: %v", err)
						m.mu.Unlock()
						m.reconnect()
						continue
					}
					log.Printf("[WebSocket] Successfully sent app details for %s", app.Name)
				} else {
					log.Printf("[WebSocket] Connection is nil, attempting to reconnect")
					m.mu.Unlock()
					m.reconnect()
					continue
				}
				m.mu.Unlock()
			}
		}
	}
}
