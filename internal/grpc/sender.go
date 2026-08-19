package grpc

import (
	"sync"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
)

// GlobalClient is the singleton gRPC client used throughout the CLI.
var (
	globalClient     *Client
	globalClientOnce sync.Once
	globalClientMu   sync.RWMutex
)

// InitGlobalClient initializes the global gRPC client. Must be called once at startup.
func InitGlobalClient() *Client {
	globalClientOnce.Do(func() {
		globalClient = NewClient()
	})
	return globalClient
}

// GetClient returns the global gRPC client instance.
func GetClient() *Client {
	globalClientMu.RLock()
	defer globalClientMu.RUnlock()
	return globalClient
}

// SetClient sets the global gRPC client instance (for testing).
func SetClient(c *Client) {
	globalClientMu.Lock()
	defer globalClientMu.Unlock()
	globalClient = c
}

// sendWithTemporaryClient creates a short-lived connection, sends the event, and closes.
// Used by CLI commands when the monitor is not running.
func sendWithTemporaryClient(event *pb.ApplicationEvent) {
	logs.InfoFile("grpc", "[gRPC] Creating temporary connection to send event: action=%s", event.GetAction())
	c := NewClient()
	if err := c.Connect(); err != nil {
		logs.ErrorFile("grpc", "[gRPC] Failed to create temporary connection: %v", err)
		return
	}
	defer c.Close()

	c.SendEvent(event)
}

// ReportEvent sends an event synchronously. Used by CLI commands that need
// to ensure the event is sent before the process exits.
func ReportEvent(appID, appName, action string, success bool, errMsg string, pid int, mode, version string) {
	// Initialize server to ensure server ID is available
	if err := server.Initialize(); err != nil {
		logs.ErrorFile("grpc", "[gRPC] Failed to initialize server for event: %v", err)
		return
	}

	event := NewApplicationEvent(appID, appName, action, success, errMsg, pid, mode, version)

	c := GetClient()
	if c != nil && c.IsConnected() {
		c.SendEvent(event)
		return
	}

	// No global client available, use temporary connection
	sendWithTemporaryClient(event)
}

// ReportEventAsync sends an event asynchronously. Only use this from long-running
// processes (monitor mode). For CLI commands, use ReportEvent directly.
func ReportEventAsync(appID, appName, action string, success bool, errMsg string, pid int, mode, version string) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logs.ErrorFile("grpc", "[gRPC] Panic in async event send: %v", r)
			}
		}()
		ReportEvent(appID, appName, action, success, errMsg, pid, mode, version)
	}()
}
