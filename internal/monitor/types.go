package monitor

import (
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
)

// MonitorService Interfaces following Interface Segregation Principle
type MonitorService interface {
	StartMonitoring() error
	StopMonitoring() error
	SendCommand(cmd Command) error
	SendMessage(msgType string, payload interface{}) error
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
	CollectServerMetrics() (*server.Metrics, error)
	CollectAppLogs() ([]logs.LogEntry, error)
	CollectSelfLogs() ([]logs.LogEntry, error)
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
	ID          string           `json:"id"`
	ServerID    string           `json:"server_id"`
	Name        string           `json:"name"`
	Status      string           `json:"status"`
	Language    builder.Language `json:"language"`
	Port        int              `json:"port"`
	BuildStatus string           `json:"buildStatus"`
	PID         int              `json:"pid"`
	Uptime      string           `json:"uptime"`
	CreatedAt   time.Time        `json:"createdAt"`
	UpdatedAt   time.Time        `json:"updatedAt"`
}

type Session struct {
	SessionID string    `json:"sessionID"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}
