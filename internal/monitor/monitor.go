package monitor

import (
	"fmt"
	"log"
	"time"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/gorilla/websocket"
)

type AppMetrics struct {
	AppName     string    `json:"appName"`
	CPUUsage    float64   `json:"cpuUsage"`
	MemoryUsage float64   `json:"memoryUsage"`
	Status      string    `json:"status"`
	Timestamp   time.Time `json:"timestamp"`
	PID         int       `json:"pid"`
	Uptime      string    `json:"uptime"`
}

type Command struct {
	Type    string `json:"type"` // "start", "stop", "restart"
	AppName string `json:"appName"`
}

var (
	wsConn *websocket.Conn
)

func StartMonitoring(apiKey string) {
	// Connect to WebSocket server
	wsURL := fmt.Sprintf("wss://gophel.anophel.com/api/v1/gophel/ws?apiKey=%s", apiKey)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		log.Printf("Failed to connect to WebSocket: %v", err)
		return
	}
	wsConn = conn
	defer conn.Close()

	// Start goroutine to handle incoming commands
	go handleCommands(conn)

	// Start sending metrics
	for {
		metrics := collectMetrics()
		err := conn.WriteJSON(metrics)
		if err != nil {
			log.Printf("Error sending metrics: %v", err)
			break
		}
		time.Sleep(5 * time.Second) // Send metrics every 5 seconds
	}
}

func collectMetrics() []AppMetrics {
	var metrics []AppMetrics

	// Get list of all apps
	apps := app.Manager.ListApplications()

	for _, appInfo := range apps {
		// Get detailed status for each app
		status, err := app.Manager.StatusApplication(appInfo.ID)
		if err != nil {
			log.Printf("Error getting status for app %s: %v", appInfo.Name, err)
			continue
		}

		// Create metrics for the app
		appMetrics := AppMetrics{
			AppName:     appInfo.Name,
			Status:      appInfo.Status,
			PID:         appInfo.PID,
			Uptime:      appInfo.Uptime,
			Timestamp:   time.Now(),
			CPUUsage:    status.CPUUsage,
			MemoryUsage: float64(status.RAMUsage) / (1024 * 1024), // Convert to MB
		}

		metrics = append(metrics, appMetrics)
	}

	return metrics
}

func handleCommands(conn *websocket.Conn) {
	for {
		var cmd Command
		err := conn.ReadJSON(&cmd)
		if err != nil {
			log.Printf("Error reading command: %v", err)
			continue
		}

		// Find the app by name
		apps := app.Manager.ListApplications()
		var targetAppID string
		for _, app := range apps {
			if app.Name == cmd.AppName {
				targetAppID = app.ID
				break
			}
		}

		if targetAppID == "" {
			log.Printf("App not found: %s", cmd.AppName)
			continue
		}

		switch cmd.Type {
		case "start":
			err = app.Manager.StartApplication(targetAppID, 0, cmd.AppName)
		case "stop":
			err = app.Manager.StopApplication(targetAppID)
		case "restart":
			err = app.Manager.RestartApplication(targetAppID)
		default:
			log.Printf("Unknown command type: %s", cmd.Type)
			continue
		}

		if err != nil {
			log.Printf("Error executing command %s on app %s: %v", cmd.Type, cmd.AppName, err)
		}
	}
}

func SendCommand(cmd Command) error {
	if wsConn == nil {
		return fmt.Errorf("WebSocket connection not established")
	}
	return wsConn.WriteJSON(cmd)
}
