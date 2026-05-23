package monitor

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/phelix/internal/server"
)

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

	sessionFile := filepath.Join(os.Getenv("HOME"), ".phelix", "session.json")
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

func (m *monitorService) StopMonitoring() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	close(m.stopChan)
	return m.connector.Close()
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
