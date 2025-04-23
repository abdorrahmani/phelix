package monitor

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/gorilla/websocket"
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
}

// monitorService implements the MonitorService interface
type monitorService struct {
	conn     *websocket.Conn
	stopChan chan struct{}
	mu       sync.Mutex
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

	wsURL := fmt.Sprintf("wss://gophel.anophel.com/api/v1/gophel/ws?apiKey=%s", apiKey)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to WebSocket: %w", err)
	}

	m.conn = conn

	// Start goroutines for handling commands and sending metrics
	go m.handleCommands()
	go m.sendMetrics()

	return nil
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
			var cmd Command
			if err := m.conn.ReadJSON(&cmd); err != nil {
				log.Printf("Error reading command: %v", err)
				continue
			}

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
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			metrics := m.collectMetrics()
			if err := m.conn.WriteJSON(metrics); err != nil {
				log.Printf("Error sending metrics: %v", err)
			}
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
		})
	}

	return metrics
}
