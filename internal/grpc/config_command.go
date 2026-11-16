package grpc

import (
	"sync"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
)

const (
	AppConfigCommandRead     = "config_read_v2"
	AppConfigCommandValidate = "config_validate_v2"
	AppConfigCommandSave     = "config_save_v2"
	AppConfigCommandCreate   = "config_create_v2"
)

// IsAppConfigCommand identifies configuration commands.
func IsAppConfigCommand(commandType string) bool {
	switch commandType {
	case AppConfigCommandRead, AppConfigCommandValidate, AppConfigCommandSave, AppConfigCommandCreate:
		return true
	}
	return false
}

type AppConfigHandlerFunc func(req *pb.MonitorCommandRequest)

var (
	appConfigHandlerMu sync.RWMutex
	appConfigHandler   AppConfigHandlerFunc
)

// SetAppConfigHandler registers the configuration command handler.
func SetAppConfigHandler(h AppConfigHandlerFunc) {
	appConfigHandlerMu.Lock()
	defer appConfigHandlerMu.Unlock()
	appConfigHandler = h
}

func getAppConfigHandler() AppConfigHandlerFunc {
	appConfigHandlerMu.RLock()
	defer appConfigHandlerMu.RUnlock()
	return appConfigHandler
}

var pendingAppConfigWG sync.WaitGroup
var activeAppConfigCommands = make(chan struct{}, 8)

// WaitPendingAppConfigCommands waits for configuration requests to finish.
func WaitPendingAppConfigCommands(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		pendingAppConfigWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logs.WarningFile("grpc", "[gRPC Config] shutdown drain timed out after %s", timeout)
	}
}

// handleAppConfigCommand keeps configuration requests from delaying monitoring.
func (c *Client) handleAppConfigCommand(req *pb.MonitorCommandRequest) {
	handler := getAppConfigHandler()
	if handler == nil {
		logs.WarningFile("grpc", "[gRPC Config] configuration handler is not registered; ignoring %s", req.GetType())
		return
	}
	select {
	case activeAppConfigCommands <- struct{}{}:
	default:
		logs.WarningFile("grpc", "[gRPC Config] configuration requests are busy; request %s will need a retry", req.GetRequestId())
		return
	}
	pendingAppConfigWG.Add(1)
	go func() {
		defer pendingAppConfigWG.Done()
		defer func() { <-activeAppConfigCommands }()
		handler(req)
	}()
}
