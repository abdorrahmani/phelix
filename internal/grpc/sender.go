package grpc

import (
	"sync"

	"github.com/abdorrahmani/phelix/internal/app"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
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
//
// The returned error describes why delivery failed (dial failure, rejection,
// RPC error); callers that want to tell the user their dashboard is stale use
// it directly.
func sendWithTemporaryClient(event *pb.ApplicationEvent) error {
	logs.InfoFile("grpc", "[gRPC] Creating temporary connection to send event: action=%s", event.GetAction())
	c := NewClient()
	if err := c.Connect(); err != nil {
		logs.ErrorFile("grpc", "[gRPC] Failed to create temporary connection: %v", err)
		return phelixerr.Wrap(phelixerr.CodeConnection, "failed to connect to the dashboard backend", err)
	}
	defer c.Close()

	return c.SendEventChecked(event)
}

// ReportEvent sends an event synchronously. Used by CLI commands that need
// to ensure the event is sent before the process exits.
//
// This variant keeps the historical fire-and-forget semantics: nothing is
// echoed to the terminal and failures only land in phelix.log. Interactive
// commands should prefer ReportEventResult, which reports why the backend
// was not updated.
func ReportEvent(appID, appName, action string, success bool, errMsg string, pid int, mode, version string) {
	_ = ReportEventResult(appID, appName, action, success, errMsg, pid, mode, version)
}

// ReportEventResult sends an application event synchronously like ReportEvent,
// but tells the caller whether the backend actually received it. A nil return
// means the backend accepted the event (e.g. the app really was deleted from
// its database); non-nil explains why the dashboard is now out of date.
func ReportEventResult(appID, appName, action string, success bool, errMsg string, pid int, mode, version string) error {
	// Unwatched apps do not participate in backend monitoring/reporting: the
	// event is intentionally not sent, which is not a staleness condition the
	// caller should warn about.
	if !app.IsWatched(appID, appName) {
		return nil
	}

	// Without a session there is nothing to attribute the event to and the
	// backend would reject it. Skip the upload entirely — the command itself
	// already ran successfully.
	if !sessionAvailable() {
		return phelixerr.New(
			phelixerr.CodeUnauthenticated,
			"not logged in; run 'phelix auth login' to sync with the dashboard",
		)
	}

	// Initialize server to ensure server ID is available
	if err := server.Initialize(); err != nil {
		logs.ErrorFile("grpc", "[gRPC] Failed to initialize server for event: %v", err)
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to initialize server identity", err)
	}

	event := NewApplicationEvent(appID, appName, action, success, errMsg, pid, mode, version)

	c := GetClient()
	if c != nil && c.IsConnected() {
		return c.SendEventChecked(event)
	}

	// No global client available, use temporary connection
	return sendWithTemporaryClient(event)
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
