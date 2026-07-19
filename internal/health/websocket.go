package health

import (
	"log"
	"time"
)

// MonitorWebSocketAdapter adapts the health module to send data through the monitor service
type MonitorWebSocketAdapter struct {
	// This will be populated by the monitor service
	sendFunc func(msgType string, payload interface{}) error
}

// NewMonitorWebSocketAdapter creates a new adapter
func NewMonitorWebSocketAdapter() *MonitorWebSocketAdapter {
	return &MonitorWebSocketAdapter{}
}

// SetSendFunc sets the function used to send messages
func (a *MonitorWebSocketAdapter) SetSendFunc(sendFunc func(msgType string, payload interface{}) error) {
	a.sendFunc = sendFunc
}

// SendHealthCheckResult sends a health check result to the backend
func (a *MonitorWebSocketAdapter) SendHealthCheckResult(result *HealthCheckResult, appID string, appName string) error {
	if a.sendFunc == nil {
		return nil // Not connected or not configured
	}

	payload := map[string]interface{}{
		"app_id":        appID,
		"app_name":      appName,
		"endpoint_name": result.EndpointName,
		"url":           result.URL,
		"status":        result.Status,
		"status_code":   result.StatusCode,
		"latency_ms":    result.LatencyMs,
		"checked_at":    result.CheckedAt,
		"error":         result.Error,
	}

	if err := a.sendFunc("health_check_result", payload); err != nil {
		log.Printf("[Health WebSocket] Failed to send health check result: %v", err)
		return err
	}

	return nil
}

// SendAutoRestartEvent sends an auto-restart event to the backend
func (a *MonitorWebSocketAdapter) SendAutoRestartEvent(record *AutoRestartRecord) error {
	if a.sendFunc == nil {
		return nil // Not connected or not configured
	}

	payload := map[string]interface{}{
		"app_id":               record.AppID,
		"app_name":             record.AppName,
		"reason":               record.Reason,
		"exit_code":            record.ExitCode,
		"backoff_next_seconds": record.BackoffNextSeconds,
		"restarted_at":         record.RestartedAt,
		"crash_count_24h":      record.CrashCount24h,
	}

	if err := a.sendFunc("auto_restart", payload); err != nil {
		log.Printf("[Health WebSocket] Failed to send auto-restart event: %v", err)
		return err
	}

	return nil
}

// IsConnected returns whether the WebSocket is connected
func (a *MonitorWebSocketAdapter) IsConnected() bool {
	return a.sendFunc != nil
}

// NoOpWebSocketClient is a no-op implementation for when WebSocket is not available
type NoOpWebSocketClient struct{}

func (n *NoOpWebSocketClient) SendHealthCheckResult(result *HealthCheckResult, appID string, appName string) error {
	return nil
}

func (n *NoOpWebSocketClient) SendAutoRestartEvent(record *AutoRestartRecord) error {
	return nil
}

func (n *NoOpWebSocketClient) IsConnected() bool {
	return false
}

// BufferingWebSocketClient buffers messages if WebSocket is disconnected
type BufferingWebSocketClient struct {
	underlying WebSocketClient
	buffer     chan interface{}
	stopChan   chan struct{}
	maxBuffer  int
}

// NewBufferingWebSocketClient creates a buffering wrapper
func NewBufferingWebSocketClient(underlying WebSocketClient, maxBuffer int) *BufferingWebSocketClient {
	return &BufferingWebSocketClient{
		underlying: underlying,
		buffer:     make(chan interface{}, maxBuffer),
		stopChan:   make(chan struct{}),
		maxBuffer:  maxBuffer,
	}
}

// SendHealthCheckResult sends or buffers a health check result
func (b *BufferingWebSocketClient) SendHealthCheckResult(result *HealthCheckResult, appID string, appName string) error {
	if b.underlying.IsConnected() {
		return b.underlying.SendHealthCheckResult(result, appID, appName)
	}

	// Buffer the message
	select {
	case b.buffer <- map[string]interface{}{
		"type":    "health_check_result",
		"result":  result,
		"appID":   appID,
		"appName": appName,
	}:
		return nil
	default:
		// Buffer full, drop oldest
		select {
		case <-b.buffer:
		default:
		}
		b.buffer <- map[string]interface{}{
			"type":    "health_check_result",
			"result":  result,
			"appID":   appID,
			"appName": appName,
		}
		return nil
	}
}

// SendAutoRestartEvent sends or buffers an auto-restart event
func (b *BufferingWebSocketClient) SendAutoRestartEvent(record *AutoRestartRecord) error {
	if b.underlying.IsConnected() {
		return b.underlying.SendAutoRestartEvent(record)
	}

	// Buffer the message
	select {
	case b.buffer <- map[string]interface{}{
		"type":   "auto_restart",
		"record": record,
	}:
		return nil
	default:
		// Buffer full, drop oldest
		select {
		case <-b.buffer:
		default:
		}
		b.buffer <- map[string]interface{}{
			"type":   "auto_restart",
			"record": record,
		}
		return nil
	}
}

// IsConnected returns whether the underlying WebSocket is connected
func (b *BufferingWebSocketClient) IsConnected() bool {
	return b.underlying.IsConnected()
}

// FlushBuffer flushes the buffer to the backend
func (b *BufferingWebSocketClient) FlushBuffer() error {
	for {
		select {
		case msg := <-b.buffer:
			if msgMap, ok := msg.(map[string]interface{}); ok {
				msgType := msgMap["type"].(string)
				switch msgType {
				case "health_check_result":
					result := msgMap["result"].(*HealthCheckResult)
					appID := msgMap["appID"].(string)
					appName := msgMap["appName"].(string)
					if err := b.underlying.SendHealthCheckResult(result, appID, appName); err != nil {
						return err
					}
				case "auto_restart":
					record := msgMap["record"].(*AutoRestartRecord)
					if err := b.underlying.SendAutoRestartEvent(record); err != nil {
						return err
					}
				}
			}
		default:
			return nil
		}
	}
}

// Stop stops the buffering client
func (b *BufferingWebSocketClient) Stop() {
	close(b.stopChan)
}

// HealthWebSocketManager manages WebSocket communication for health checks
type HealthWebSocketManager struct {
	adapter    *MonitorWebSocketAdapter
	buffering  *BufferingWebSocketClient
	isActive   bool
	stopChan   chan struct{}
	tickerChan chan struct{}
}

// NewHealthWebSocketManager creates a new manager
func NewHealthWebSocketManager() *HealthWebSocketManager {
	adapter := NewMonitorWebSocketAdapter()
	buffering := NewBufferingWebSocketClient(adapter, 1000)

	mgr := &HealthWebSocketManager{
		adapter:    adapter,
		buffering:  buffering,
		stopChan:   make(chan struct{}),
		tickerChan: make(chan struct{}),
	}

	// Start a periodic flush goroutine
	go mgr.flushLoop()

	return mgr
}

// SetSendFunc sets the send function from the monitor service
func (m *HealthWebSocketManager) SetSendFunc(sendFunc func(msgType string, payload interface{}) error) {
	m.adapter.SetSendFunc(sendFunc)
	m.isActive = true

	// Try to flush buffered messages
	_ = m.buffering.FlushBuffer()
}

// SendHealthCheckResult sends a health check result
func (m *HealthWebSocketManager) SendHealthCheckResult(result *HealthCheckResult, appID string, appName string) error {
	return m.buffering.SendHealthCheckResult(result, appID, appName)
}

// SendAutoRestartEvent sends an auto-restart event
func (m *HealthWebSocketManager) SendAutoRestartEvent(record *AutoRestartRecord) error {
	return m.buffering.SendAutoRestartEvent(record)
}

// IsConnected returns whether WebSocket is connected
func (m *HealthWebSocketManager) IsConnected() bool {
	return m.adapter.IsConnected()
}

// flushLoop periodically attempts to flush the buffer
func (m *HealthWebSocketManager) flushLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			if m.adapter.IsConnected() {
				_ = m.buffering.FlushBuffer()
			}
		}
	}
}

// Stop stops the manager
func (m *HealthWebSocketManager) Stop() {
	close(m.stopChan)
	m.buffering.Stop()
}
