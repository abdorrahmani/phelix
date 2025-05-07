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
		HandshakeTimeout: 45 * time.Second,
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

	if err := m.connector.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
		return fmt.Errorf("error setting read deadline: %w", err)
	}

	if err := m.connector.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return fmt.Errorf("error setting write deadline: %w", err)
	}

	m.connector.SetPongHandler(func(string) error {
		return m.connector.SetReadDeadline(time.Now().Add(60 * time.Second))
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
				log.Printf("Error reading command: %v", err)
				m.reconnect()
				continue
			}

			if err := m.commandExecutor.Execute(cmd); err != nil {
				log.Printf("Error executing command: %v", err)
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
			if err := m.connector.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				m.mu.Unlock()
				m.reconnect()
				continue
			}

			if err := m.connector.WriteMessage(websocket.PingMessage, nil); err != nil {
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
		time.Sleep(reconnectDelay)
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
