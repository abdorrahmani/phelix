package monitor

import (
	"encoding/json"
	"log"
	"time"

	"github.com/abdorrahmani/phelix/internal/server"
)

func (m *monitorService) sendServerInfo() error {
	serverInfo := server.GetServerInfo()
	message := map[string]interface{}{
		"type":    "servers",
		"payload": serverInfo,
	}
	return m.connector.WriteJSON(message)
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
			m.sendSelfLogs()
			m.mu.Unlock()

			// Reset write deadline after sending metrics
			if err := m.connector.SetWriteDeadline(time.Time{}); err != nil {
				log.Printf("Error resetting write deadline: %v", err)
			}
		}
	}
}

func (m *monitorService) sendSelfLogs() {
	entries, err := m.metricsCollector.CollectSelfLogs()
	if err != nil {
		log.Printf("Error collecting self logs: %v", err)
		return
	}

	for _, entry := range entries {
		message := map[string]any{
			"type":    "self_logs",
			"payload": entry,
		}
		if err := m.connector.WriteJSON(message); err != nil {
			log.Printf("Error sending self log: %v", err)
			return
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
				log.Printf("WebSocket read failed, attempting to reconnect: %v", err)
				m.reconnect()
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
					m.sendSelfLogs()
					m.mu.Unlock()
				}()

			case "pong":
				// Handle pong message
				log.Printf("Received pong response")

			case "apps", "metrics", "servers", "server_metrics", "app_logs", "self_logs":
				// These are data request commands, no payload required
				// They are handled by the metrics collector
				continue

			default:
				log.Printf("Unknown message type: %s", msgType)
			}
		}
	}
}
