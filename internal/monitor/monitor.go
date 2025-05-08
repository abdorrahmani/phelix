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
	"github.com/abdorrahmani/gophel/internal/server"
	"github.com/gorilla/websocket"
)

// Constants for configuration
const (
	pingInterval         = 30 * time.Second
	metricsInterval      = 2 * time.Second
	reconnectDelay       = 5 * time.Second
	maxReconnectAttempts = 3
	wsURL                = "wss://gophel.anophel.com/api/v1/gophel/ws"
	readTimeout          = 60 * time.Second
	writeTimeout         = 10 * time.Second
	handshakeTimeout     = 45 * time.Second
)

// Interfaces following Interface Segregation Principle
type MonitorService interface {
	StartMonitoring() error
	StopMonitoring() error
	SendCommand(cmd Command) error
}

type WebSocketConnector interface {
	Connect(token, sessionID string) error
	Close() error
	WriteJSON(v interface{}) error
	ReadJSON(v interface{}) error
	WriteMessage(messageType int, data []byte) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
	SetPongHandler(h func(string) error)
}

type MetricsCollector interface {
	CollectAppMetrics() []AppMetrics
	CollectAppDetails() []AppDetails
	CollectServerMetrics() (*server.ServerMetrics, error)
}

type CommandExecutor interface {
	Execute(cmd Command) error
}

// Core types
type Command struct {
	Type    string `json:"type"`
	AppName string `json:"appName"`
}

type AppMetrics struct {
	AppID       string  `json:"appID"`
	ServerID    string  `json:"server_id"`
	CPUUsage    float64 `json:"cpuUsage"`
	MemoryUsage uint64  `json:"memoryUsage"`
}

type AppDetails struct {
	ID          string    `json:"id"`
	ServerID    string    `json:"server_id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Port        int       `json:"port"`
	BuildStatus string    `json:"buildStatus"`
	PID         int       `json:"pid"`
	Uptime      string    `json:"uptime"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type Session struct {
	SessionID string    `json:"sessionID"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// WebSocket implementation
type websocketConn struct {
	conn *websocket.Conn
}

func (w *websocketConn) Connect(token, sessionID string) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
		ReadBufferSize:   32768,
		WriteBufferSize:  32768,
	}
	url := fmt.Sprintf("%s?token=%s&sessionID=%s", wsURL, token, sessionID)
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to WebSocket: %w", err)
	}
	w.conn = conn
	return nil
}

func (w *websocketConn) Close() error {
	if w.conn != nil {
		return w.conn.Close()
	}
	return nil
}

func (w *websocketConn) WriteJSON(v interface{}) error {
	return w.conn.WriteJSON(v)
}

func (w *websocketConn) ReadJSON(v interface{}) error {
	return w.conn.ReadJSON(v)
}

func (w *websocketConn) WriteMessage(messageType int, data []byte) error {
	return w.conn.WriteMessage(messageType, data)
}

func (w *websocketConn) SetReadDeadline(t time.Time) error {
	return w.conn.SetReadDeadline(t)
}

func (w *websocketConn) SetWriteDeadline(t time.Time) error {
	return w.conn.SetWriteDeadline(t)
}

func (w *websocketConn) SetPongHandler(h func(string) error) {
	w.conn.SetPongHandler(h)
}

// Metrics collector implementation
type appMetricsCollector struct{}

func (c *appMetricsCollector) CollectAppMetrics() []AppMetrics {
	var metrics []AppMetrics
	apps := app.Manager.ListApplications()
	serverID := server.GetServerID()

	for _, appInfo := range apps {
		status, err := app.Manager.StatusApplication(appInfo.ID)
		if err != nil {
			log.Printf("Error getting status for app %s: %v", appInfo.Name, err)
			continue
		}

		metrics = append(metrics, AppMetrics{
			AppID:       appInfo.ID,
			ServerID:    serverID,
			CPUUsage:    status.CPUUsage,
			MemoryUsage: status.RAMUsage,
		})
	}
	return metrics
}

func (c *appMetricsCollector) CollectAppDetails() []AppDetails {
	var apps []AppDetails
	appList := app.Manager.ListApplications()
	serverID := server.GetServerID()

	for _, appInfo := range appList {
		apps = append(apps, AppDetails{
			ID:          appInfo.ID,
			ServerID:    serverID,
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

func (c *appMetricsCollector) CollectServerMetrics() (*server.ServerMetrics, error) {
	return server.CollectMetrics()
}

// Command executor implementation
type appCommandExecutor struct{}

func (e *appCommandExecutor) Execute(cmd Command) error {
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

	switch cmd.Type {
	case "start":
		return app.Manager.StartApplication(targetAppID, 0, cmd.AppName)
	case "stop":
		return app.Manager.StopApplication(targetAppID)
	case "restart":
		return app.Manager.RestartApplication(targetAppID)
	default:
		return fmt.Errorf("unknown command type: %s", cmd.Type)
	}
}

// Main monitor service
type monitorService struct {
	connector         WebSocketConnector
	metricsCollector  MetricsCollector
	commandExecutor   CommandExecutor
	session           Session
	stopChan          chan struct{}
	mu                sync.Mutex
	reconnectAttempts int
}

func NewMonitorService() MonitorService {
	return &monitorService{
		connector:        &websocketConn{},
		metricsCollector: &appMetricsCollector{},
		commandExecutor:  &appCommandExecutor{},
		stopChan:         make(chan struct{}),
	}
}

func (m *monitorService) StartMonitoring() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.initialize(); err != nil {
		return err
	}

	if err := m.connect(); err != nil {
		return err
	}

	if err := m.sendServerInfo(); err != nil {
		log.Printf("Warning: Failed to send server info: %v", err)
	}

	go m.handleCommands()
	go m.sendMetrics()
	go m.keepAlive()

	m.reconnectAttempts = 0
	return nil
}

func (m *monitorService) initialize() error {
	if err := server.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize server info: %w", err)
	}

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

	return nil
}

func (m *monitorService) connect() error {
	if err := m.connector.Connect(m.session.Token, m.session.SessionID); err != nil {
		return err
	}

	// Set initial read deadline
	if err := m.connector.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		return fmt.Errorf("error setting read deadline: %w", err)
	}

	// Set initial write deadline
	if err := m.connector.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("error setting write deadline: %w", err)
	}

	// Set pong handler to extend read deadline
	m.connector.SetPongHandler(func(string) error {
		return m.connector.SetReadDeadline(time.Now().Add(readTimeout))
	})

	return nil
}

func (m *monitorService) sendServerInfo() error {
	serverInfo := server.GetServerInfo()
	message := map[string]interface{}{
		"type":    "servers",
		"payload": serverInfo,
	}
	return m.connector.WriteJSON(message)
}

func (m *monitorService) StopMonitoring() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	close(m.stopChan)
	return m.connector.Close()
}

func (m *monitorService) SendCommand(cmd Command) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connector.WriteJSON(cmd)
}

func (m *monitorService) handleCommands() {
	for {
		select {
		case <-m.stopChan:
			return
		default:
			var cmd Command
			if err := m.connector.ReadJSON(&cmd); err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("WebSocket connection closed unexpectedly: %v", err)
					m.reconnect()
				} else {
					log.Printf("Error reading command: %v", err)
				}
				continue
			}

			// Log received command for debugging
			log.Printf("Received command: type=%s, appName=%s", cmd.Type, cmd.AppName)

			// Handle different command types
			switch cmd.Type {
			case "start", "stop", "restart":
				// These are control commands that require appName
				if cmd.AppName == "" {
					log.Printf("Invalid control command: empty app name for command type: %s", cmd.Type)
					response := map[string]interface{}{
						"type":      "command_response",
						"status":    "error",
						"error":     "empty app name",
						"timestamp": time.Now(),
					}
					m.mu.Lock()
					if err := m.connector.WriteJSON(response); err != nil {
						log.Printf("Error sending command response: %v", err)
						m.reconnect()
					}
					m.mu.Unlock()
					continue
				}

				err := m.commandExecutor.Execute(cmd)
				response := map[string]interface{}{
					"type":      "command_response",
					"appName":   cmd.AppName,
					"status":    "success",
					"timestamp": time.Now(),
				}

				if err != nil {
					response["status"] = "error"
					response["error"] = err.Error()
				}

				m.mu.Lock()
				if err := m.connector.WriteJSON(response); err != nil {
					log.Printf("Error sending command response: %v", err)
					m.reconnect()
				}
				m.mu.Unlock()

			case "apps", "metrics", "servers", "server_metrics":
				// These are data request commands, no appName required
				// They are handled by the metrics collector
				continue

			case "ping":
				response := map[string]interface{}{
					"type":      "pong",
					"timestamp": time.Now(),
				}
				m.mu.Lock()
				if err := m.connector.WriteJSON(response); err != nil {
					log.Printf("Error sending pong response: %v", err)
					m.reconnect()
				}
				m.mu.Unlock()

			default:
				log.Printf("Unknown command type: %s", cmd.Type)
			}
		}
	}
}

func (m *monitorService) keepAlive() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.mu.Lock()
			if err := m.connector.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
				m.mu.Unlock()
				m.reconnect()
				continue
			}

			// Send a ping message with timestamp
			pingMsg := map[string]interface{}{
				"type":      "ping",
				"timestamp": time.Now(),
			}

			if err := m.connector.WriteJSON(pingMsg); err != nil {
				m.mu.Unlock()
				m.reconnect()
				continue
			}

			if err := m.connector.SetWriteDeadline(time.Time{}); err != nil {
				log.Printf("Error resetting write deadline: %v", err)
			}
			m.mu.Unlock()
		}
	}
}

func (m *monitorService) reconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.reconnectAttempts >= maxReconnectAttempts {
		log.Printf("Max reconnection attempts reached, waiting before retry")
		time.Sleep(reconnectDelay * 2) // Double the delay after max attempts
		m.reconnectAttempts = 0
	}

	if err := m.connector.Close(); err != nil {
		log.Printf("Error closing connection: %v", err)
	}

	time.Sleep(reconnectDelay)
	if err := m.connect(); err != nil {
		log.Printf("Failed to reconnect: %v", err)
		m.reconnectAttempts++
		return
	}

	log.Printf("Successfully reconnected to WebSocket server")
	m.reconnectAttempts = 0
}

func (m *monitorService) sendMetrics() {
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.sendServerMetrics()
			m.sendAppMetrics()
			m.sendAppDetails()
		}
	}
}

func (m *monitorService) sendServerMetrics() {
	metrics, err := m.metricsCollector.CollectServerMetrics()
	if err != nil {
		log.Printf("Error collecting server metrics: %v", err)
		return
	}

	message := map[string]interface{}{
		"type":    "server_metrics",
		"payload": metrics,
	}

	m.mu.Lock()
	if err := m.connector.WriteJSON(message); err != nil {
		log.Printf("Error sending server metrics: %v", err)
	}
	m.mu.Unlock()
}

func (m *monitorService) sendAppMetrics() {
	metrics := m.metricsCollector.CollectAppMetrics()
	for _, metric := range metrics {
		message := map[string]interface{}{
			"type":    "metrics",
			"payload": metric,
		}

		m.mu.Lock()
		if err := m.connector.WriteJSON(message); err != nil {
			log.Printf("Error sending metrics: %v", err)
			m.mu.Unlock()
			m.reconnect()
			continue
		}
		m.mu.Unlock()
	}
}

func (m *monitorService) sendAppDetails() {
	apps := m.metricsCollector.CollectAppDetails()
	for _, app := range apps {
		message := map[string]interface{}{
			"type":    "apps",
			"payload": app,
		}

		m.mu.Lock()
		if err := m.connector.WriteJSON(message); err != nil {
			log.Printf("Error sending app details: %v", err)
			m.mu.Unlock()
			m.reconnect()
			continue
		}
		m.mu.Unlock()
	}
}
