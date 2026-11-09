package monitor

import (
	"sync"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

const (
	// CommandRestartMonitor restarts the systemd-supervised monitor daemon.
	CommandRestartMonitor = "restart-monitor"
	// CommandRestartAgent is the legacy name for CommandRestartMonitor.
	CommandRestartAgent = "restart-agent"
)

// RestartAgentHandler triggers a detached restart of the monitor service. The
// cmd package registers the systemd implementation at daemon startup; nil
// means the capability is unavailable and every request fails closed. The full
// flow is specified in backend-contract/connection-lifecycle.md.
var RestartAgentHandler func() error

// SetRestartAgentHandler registers the monitor self-restart implementation.
func SetRestartAgentHandler(handler func() error) {
	RestartAgentHandler = handler
}

// IsRestartMonitorCommand reports whether commandType requests a monitor
// restart. Both the canonical name and the legacy alias are accepted.
func IsRestartMonitorCommand(commandType string) bool {
	return commandType == CommandRestartMonitor || commandType == CommandRestartAgent
}

// ValidateRestartMonitorRequest rejects a restart request that carries an app
// target or no request id. Restart is server-scoped and must be replay-safe.
func ValidateRestartMonitorRequest(appName, requestID string) error {
	if appName != "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"monitor restart does not target an app, got %q", appName)
	}
	if requestID == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument,
			"monitor restart requires a request_id")
	}
	return nil
}

// RestartMonitor queues a detached restart of the monitor service. It returns
// as soon as the restart is queued so the accepted result can reach the
// backend before systemd stops this process.
func RestartMonitor() error {
	if RestartAgentHandler == nil {
		return phelixerr.New(phelixerr.CodeUnimplemented,
			"monitor restart is not available in this build")
	}
	return RestartAgentHandler()
}

// restartRequestCache remembers the outcome of a request id so a replayed
// command never triggers a second restart.
type restartRequestCache struct {
	mu      sync.Mutex
	results map[string]RestartOutcome
}

// RestartOutcome is the terminal result of one restart request.
type RestartOutcome struct {
	ErrorText string
	ErrorCode string
}

var restartRequests restartRequestCache

// BeginRestartRequest returns a cached outcome when requestID was already
// handled, otherwise it marks the request as seen and returns ok=false.
func BeginRestartRequest(requestID string) (RestartOutcome, bool) {
	restartRequests.mu.Lock()
	defer restartRequests.mu.Unlock()
	if outcome, seen := restartRequests.results[requestID]; seen {
		return outcome, true
	}
	if restartRequests.results == nil {
		restartRequests.results = make(map[string]RestartOutcome)
	}
	restartRequests.results[requestID] = RestartOutcome{}
	return RestartOutcome{}, false
}

// FinishRestartRequest stores the terminal outcome for a request id.
func FinishRestartRequest(requestID string, err error) RestartOutcome {
	outcome := RestartOutcome{}
	if err != nil {
		outcome.ErrorText = phelixerr.Redact(err.Error())
		if structured := phelixerr.AsError(err); structured != nil {
			outcome.ErrorCode = string(structured.Code)
		} else {
			outcome.ErrorCode = string(phelixerr.CodeUnknown)
		}
	}
	restartRequests.mu.Lock()
	if restartRequests.results == nil {
		restartRequests.results = make(map[string]RestartOutcome)
	}
	restartRequests.results[requestID] = outcome
	restartRequests.mu.Unlock()
	return outcome
}
