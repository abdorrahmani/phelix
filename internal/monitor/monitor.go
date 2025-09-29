package monitor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/abdorrahmani/gophel/internal/logs"
	"github.com/abdorrahmani/gophel/internal/server"
	"github.com/gorilla/websocket"
)

const (
	pingInterval         = 15 * time.Second
	metricsInterval      = 2 * time.Second
	reconnectDelay       = 5 * time.Second
	maxReconnectAttempts = 3
	wsURL                = "wss://gophel.anophel.com/api/v1/gophel/ws"
	writeTimeout         = 10 * time.Second
	handshakeTimeout     = 45 * time.Second
	pongWait             = 60 * time.Second
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
	CollectAppLogs() ([]logs.AppLogs, error)
}

type CommandExecutor interface {
	Execute(cmd Command) error
}

// CommandPayload Core types
type CommandPayload struct {
	Type    string `json:"type"`
	AppName string `json:"appName"`
}

type Command struct {
	Type    string         `json:"type"`
	Payload CommandPayload `json:"payload"`
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

func (c *appMetricsCollector) CollectAppLogs() ([]logs.AppLogs, error) {
	return logs.CollectAppLogs()
}

// Command executor implementation
type appCommandExecutor struct{}

func (e *appCommandExecutor) Execute(cmd Command) error {
	apps := app.Manager.ListApplications()
	var targetAppID string
	var targetAppName string

	// First try to find by app name
	for _, app := range apps {
		if app.Name == cmd.Payload.AppName {
			targetAppID = app.ID
			targetAppName = app.Name
			break
		}
	}

	// If not found by name, try to use the appName as ID directly
	if targetAppID == "" {
		// Check if the appName is actually an ID
		for _, app := range apps {
			if app.ID == cmd.Payload.AppName {
				targetAppID = app.ID
				targetAppName = app.Name
				break
			}
		}
	}

	if targetAppID == "" {
		return fmt.Errorf("app not found: %s", cmd.Payload.AppName)
	}

	log.Printf("Executing command '%s' for app '%s' (ID: %s)", cmd.Payload.Type, targetAppName, targetAppID)

	// Find the gophel executable in PATH
	gophelPath, err := exec.LookPath("gophel")
	if err != nil {
		return fmt.Errorf("failed to find gophel executable: %v", err)
	}

	// Create the command with the correct format: gophel commandName ID
	execCmd := exec.Command(gophelPath, cmd.Payload.Type, targetAppID)

	// Set up environment with Go variables
	env := os.Environ()
	env = append(env,
		"GOROOT=/usr/local/go",
		"GOPATH="+os.Getenv("HOME")+"/go",
	)

	// Add Go paths to PATH
	path := os.Getenv("PATH")
	goRoot := "/usr/local/go/bin"
	goPath := os.Getenv("HOME") + "/go/bin"
	env = append(env, "PATH="+goRoot+":"+goPath+":"+path)

	execCmd.Env = env

	// Set working directory to the current directory
	execCmd.Dir = "."

	// Create pipes for stdout and stderr
	stdout, err := execCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}
	stderr, err := execCmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	// Start the command
	if err := execCmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %v", err)
	}

	// Create channels to handle output
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	// Handle stdout
	go func() {
		defer close(stdoutDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			log.Printf("[Command stdout] %s", scanner.Text())
		}
	}()

	// Handle stderr
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("[Command stderr] %s", scanner.Text())
		}
	}()

	// Wait for command to complete
	if err := execCmd.Wait(); err != nil {
		return fmt.Errorf("command failed: %v", err)
	}

	// Wait for output handling to complete
	<-stdoutDone
	<-stderrDone

	return nil
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
	metricsPaused     bool
	metricsPauseMu    sync.Mutex
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
	if err := m.connector.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		return fmt.Errorf("error setting read deadline: %w", err)
	}

	// Set initial write deadline
	if err := m.connector.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("error setting write deadline: %w", err)
	}

	// Set pong handler to extend read deadline
	m.connector.SetPongHandler(func(string) error {
		return m.connector.SetReadDeadline(time.Now().Add(pongWait))
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

func (m *monitorService) pauseMetrics() {
	m.metricsPauseMu.Lock()
	m.metricsPaused = true
	m.metricsPauseMu.Unlock()
}

func (m *monitorService) resumeMetrics() {
	m.metricsPauseMu.Lock()
	m.metricsPaused = false
	m.metricsPauseMu.Unlock()
}

func (m *monitorService) sendMetrics() {
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.metricsPauseMu.Lock()
			if m.metricsPaused {
				m.metricsPauseMu.Unlock()
				continue
			}
			m.metricsPauseMu.Unlock()

			// Set write deadline before sending metrics
			if err := m.connector.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
				log.Printf("Error setting write deadline for metrics: %v", err)
				continue
			}

			m.mu.Lock()
			m.sendServerMetrics()
			m.sendAppMetrics()
			m.sendAppDetails()
			m.sendAppLogs()
			m.mu.Unlock()

			// Reset write deadline after sending metrics
			if err := m.connector.SetWriteDeadline(time.Time{}); err != nil {
				log.Printf("Error resetting write deadline: %v", err)
			}
		}
	}
}

func (m *monitorService) handleCommands() {
	for {
		select {
		case <-m.stopChan:
			return
		default:
			// Set read deadline for command handling
			if err := m.connector.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
				log.Printf("Error setting read deadline: %v", err)
				m.reconnect()
				continue
			}

			var rawMessage json.RawMessage
			if err := m.connector.ReadJSON(&rawMessage); err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("WebSocket connection closed unexpectedly: %v", err)
					m.reconnect()
				} else if err.Error() == "i/o timeout" {
					// For timeout errors, try to reconnect
					log.Printf("WebSocket read timeout, attempting to reconnect")
					m.reconnect()
				} else {
					log.Printf("Error reading message: %v", err)
				}
				continue
			}

			// Reset read deadline after successful read
			if err := m.connector.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
				log.Printf("Error resetting read deadline: %v", err)
			}

			// Parse and handle the message
			var msg map[string]interface{}
			if err := json.Unmarshal(rawMessage, &msg); err != nil {
				log.Printf("Error parsing message: %v", err)
				continue
			}

			msgType, ok := msg["type"].(string)
			if !ok {
				log.Printf("Invalid message format: missing or invalid type")
				continue
			}

			// Handle different message types
			switch msgType {
			case "command":
				payload, ok := msg["payload"].(map[string]interface{})
				if !ok {
					log.Printf("Invalid command format: missing or invalid payload")
					continue
				}

				cmdType, ok := payload["type"].(string)
				if !ok {
					log.Printf("Invalid command format: missing command type")
					continue
				}

				appName, ok := payload["appName"].(string)
				if !ok {
					log.Printf("Invalid command format: missing app name")
					continue
				}

				log.Printf("Received command: type=%s, appName=%s", cmdType, appName)

				// Pause metrics sending during command execution
				m.pauseMetrics()

				// Create command for executor
				execCmd := Command{
					Type: cmdType,
					Payload: CommandPayload{
						Type:    cmdType,
						AppName: appName,
					},
				}

				// Execute command directly
				err := m.commandExecutor.Execute(execCmd)
				response := map[string]interface{}{
					"type":      "command_response",
					"status":    "success",
					"appName":   appName,
					"command":   cmdType,
					"timestamp": time.Now(),
				}

				if err != nil {
					response["status"] = "error"
					response["error"] = err.Error()
					log.Printf("Command execution failed: %v", err)
				}

				m.mu.Lock()
				if err := m.connector.WriteJSON(response); err != nil {
					log.Printf("Error sending command response: %v", err)
					m.mu.Unlock()
					m.reconnect()
					continue
				}
				m.mu.Unlock()

				// Wait a bit for the command to fully complete
				time.Sleep(2 * time.Second)

				// Resume metrics sending
				m.resumeMetrics()
				log.Printf("Metrics sending resumed after command execution")

				// Force an immediate metrics update
				go func() {
					m.mu.Lock()
					m.sendServerMetrics()
					m.sendAppMetrics()
					m.sendAppDetails()
					m.sendAppLogs()
					m.mu.Unlock()
				}()

			case "pong":
				// Handle pong message
				log.Printf("Received pong response")

			case "apps", "metrics", "servers", "server_metrics", "app_logs":
				// These are data request commands, no payload required
				// They are handled by the metrics collector
				continue

			default:
				log.Printf("Unknown message type: %s", msgType)
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

	// Try to reconnect
	for i := 0; i < maxReconnectAttempts; i++ {
		if err := m.connect(); err != nil {
			log.Printf("Reconnection attempt %d failed: %v", i+1, err)
			time.Sleep(reconnectDelay)
			continue
		}

		// If we successfully reconnect, send server info
		if err := m.sendServerInfo(); err != nil {
			log.Printf("Warning: Failed to send server info after reconnection: %v", err)
		}

		log.Printf("Successfully reconnected to WebSocket server")
		m.reconnectAttempts = 0
		return
	}

	log.Printf("Failed to reconnect after %d attempts", maxReconnectAttempts)
	m.reconnectAttempts++
}

func (m *monitorService) sendAppLogs() {
	entries, err := m.metricsCollector.CollectAppLogs()
	if err != nil {
		log.Printf("Error collecting app logs: %v", err)
		return
	}
	for _, entry := range entries {
		message := map[string]any{
			"type":    "app_logs",
			"payload": entry,
		}
		if err := m.connector.WriteJSON(message); err != nil {
			log.Printf("Error sending app log: %v", err)
			return
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

	if err := m.connector.WriteJSON(message); err != nil {
		log.Printf("Error sending server metrics: %v", err)
	}
}

func (m *monitorService) sendAppMetrics() {
	metrics := m.metricsCollector.CollectAppMetrics()
	for _, metric := range metrics {
		message := map[string]interface{}{
			"type":    "metrics",
			"payload": metric,
		}

		if err := m.connector.WriteJSON(message); err != nil {
			log.Printf("Error sending metrics: %v", err)
			return
		}
	}
}

func (m *monitorService) sendAppDetails() {
	apps := m.metricsCollector.CollectAppDetails()
	for _, app := range apps {
		message := map[string]interface{}{
			"type":    "apps",
			"payload": app,
		}

		if err := m.connector.WriteJSON(message); err != nil {
			log.Printf("Error sending app details: %v", err)
			return
		}
	}
}
