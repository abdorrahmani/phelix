package grpc

import (
	"path/filepath"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
)

// Remote autoscaling command dispatch. autoscale_status / autoscale_history
// are reads; autoscale_enable / autoscale_disable / autoscale_pause /
// autoscale_resume toggle the app's autoscaling switches and
// autoscale_evaluate runs one synchronous evaluation through the same
// controller core the monitor loop uses. Mutations are idempotent by
// request_id through a durable ledger, exactly like the worker family, and
// every mutation passes the authorization boundary (ActionScale) before any
// ledger entry is created.
//
// The cmd package registers the real implementation (SetAutoscaleHandler).

// The autoscale dispatch compiled into this build declares its capability:
// the backend may gate autoscale_* commands on it.
func init() { RegisterCapability(CapabilityAutoscaling) }

// Autoscale command types (MonitorCommandRequest.type).
const (
	AutoscaleCommandStatus   = "autoscale_status"
	AutoscaleCommandEnable   = "autoscale_enable"
	AutoscaleCommandDisable  = "autoscale_disable"
	AutoscaleCommandPause    = "autoscale_pause"
	AutoscaleCommandResume   = "autoscale_resume"
	AutoscaleCommandEvaluate = "autoscale_evaluate"
	AutoscaleCommandHistory  = "autoscale_history"
)

// autoscaleCommandSettleTimeout bounds how long daemon shutdown waits for
// in-flight autoscale mutations to finish before the process exits.
const autoscaleCommandSettleTimeout = 15 * time.Second

// AutoscaleCommandSettleTimeout exposes the shutdown drain bound for the daemon.
func AutoscaleCommandSettleTimeout() time.Duration {
	return autoscaleCommandSettleTimeout
}

// autoscaleLedgerFile is the durable idempotency ledger for remote autoscale
// mutations, kept separate from the other ledgers.
const autoscaleLedgerFile = "remote-autoscale-ledger.json"

// AutoscaleAuthorizerFunc decides whether the remote principal may execute a
// mutating autoscale command. The daemon registers an implementation backed
// by the host authorization boundary.
type AutoscaleAuthorizerFunc func(req *pb.MonitorCommandRequest) error

var (
	autoscaleHandlerMu  sync.RWMutex
	autoscaleHandler    AutoscaleHandlerFunc
	autoscaleAuthzMu    sync.RWMutex
	autoscaleAuthorizer AutoscaleAuthorizerFunc
)

// AutoscaleHandlerFunc executes one autoscale command and returns its fully
// populated result.
type AutoscaleHandlerFunc func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult

// SetAutoscaleHandler registers the remote autoscaling implementation. The
// daemon wiring calls it before the client starts.
func SetAutoscaleHandler(h AutoscaleHandlerFunc) {
	autoscaleHandlerMu.Lock()
	defer autoscaleHandlerMu.Unlock()
	autoscaleHandler = h
}

func getAutoscaleHandler() AutoscaleHandlerFunc {
	autoscaleHandlerMu.RLock()
	defer autoscaleHandlerMu.RUnlock()
	return autoscaleHandler
}

// SetAutoscaleAuthorizer registers the mutation authorization gate.
func SetAutoscaleAuthorizer(a AutoscaleAuthorizerFunc) {
	autoscaleAuthzMu.Lock()
	defer autoscaleAuthzMu.Unlock()
	autoscaleAuthorizer = a
}

func getAutoscaleAuthorizer() AutoscaleAuthorizerFunc {
	autoscaleAuthzMu.RLock()
	defer autoscaleAuthzMu.RUnlock()
	return autoscaleAuthorizer
}

var autoscaleResults = newRollbackLedger(filepath.Join(server.DataDir(), autoscaleLedgerFile), rollbackLedgerCapacity, "autoscale",
	"the autoscale mutation may or may not have been applied; reconcile with the app's autoscale status")

// InitializeAutoscaleLedger loads the durable remote-autoscale idempotency
// ledger. Monitor startup must call it before accepting autoscale commands.
func InitializeAutoscaleLedger() error {
	return autoscaleResults.initializeAt(filepath.Join(server.DataDir(), autoscaleLedgerFile))
}

// pendingAutoscaleWG tracks in-flight mutating autoscale commands for shutdown.
var pendingAutoscaleWG sync.WaitGroup

// WaitPendingAutoscaleCommands waits for in-flight autoscale commands to
// settle, bounded by timeout.
func WaitPendingAutoscaleCommands(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		pendingAutoscaleWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logs.WarningFile("grpc", "[gRPC Autoscale] shutdown drain timed out after %s", timeout)
	}
}

// isAutoscaleCommand reports whether a command type is an autoscale command.
func isAutoscaleCommand(t string) bool {
	switch t {
	case AutoscaleCommandStatus, AutoscaleCommandEnable, AutoscaleCommandDisable,
		AutoscaleCommandPause, AutoscaleCommandResume, AutoscaleCommandEvaluate, AutoscaleCommandHistory:
		return true
	}
	return false
}

// isMutatingAutoscaleCommand reports whether the command changes runtime or
// configuration state and therefore needs the ledger and the authorizer.
func isMutatingAutoscaleCommand(t string) bool {
	return isAutoscaleCommand(t) && t != AutoscaleCommandStatus && t != AutoscaleCommandHistory
}

// validateAutoscaleCommand checks the transport-level shape of an autoscale
// command before anything executes. Autoscale commands accept no deployment
// options; evaluate and history carry no replica count.
func validateAutoscaleCommand(req *pb.MonitorCommandRequest) error {
	if req.GetRequestId() == "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires a request_id", req.GetType())
	}
	if req.GetAppName() == "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires an app_name (the app's stable ID or its name)", req.GetType())
	}
	if req.GetStrategy() != "" || req.GetVerifyDurationMs() != 0 ||
		req.GetDryRun() || req.GetTarget() != "" || req.GetMatrix() != nil || req.GetWebhook() != nil {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s command must not carry strategy, verify_duration_ms, dry_run, target, matrix or webhook options", req.GetType())
	}
	if req.GetReplicas() != 0 || req.GetReplica() != "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s command must not carry replica options; scaling counts come from the decision engine", req.GetType())
	}
	return nil
}

// handleAutoscaleCommand dispatches one autoscale command. Queries execute
// synchronously; mutations pass the authorizer, are validated and
// ledger-begun synchronously, then executed on a background goroutine with
// the terminal result persisted BEFORE the send attempt.
func (c *Client) handleAutoscaleCommand(req *pb.MonitorCommandRequest) {
	if err := validateAutoscaleCommand(req); err != nil {
		_ = sendCommandResultLogged(finalizeAutoscaleResult(req, commandErrorResult(req, err)))
		return
	}

	if !isMutatingAutoscaleCommand(req.GetType()) {
		_ = sendCommandResultLogged(executeAutoscaleCommand(req))
		return
	}

	if authorizer := getAutoscaleAuthorizer(); authorizer != nil {
		if err := authorizer(req); err != nil {
			_ = sendCommandResultLogged(finalizeAutoscaleResult(req, commandErrorResult(req, err)))
			return
		}
	}

	outcome, replay, err := autoscaleResults.begin(req)
	if err != nil {
		_ = sendCommandResultLogged(finalizeAutoscaleResult(req, commandErrorResult(req, err)))
		return
	}
	if outcome == rollbackBeginReplay {
		if sendCommandResultLogged(replay) == nil {
			if err := autoscaleResults.markDelivered(req.GetRequestId()); err != nil {
				logs.ErrorFile("grpc", "[gRPC Autoscale] failed to mark replay delivered: %v", err)
			}
		}
		return
	}

	pendingAutoscaleWG.Add(1)
	go func() {
		defer pendingAutoscaleWG.Done()
		result := executeAutoscaleCommand(req)
		if err := autoscaleResults.complete(req.GetRequestId(), result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Autoscale] result persistence failed: %v", err)
			result = finalizeAutoscaleResult(req, commandErrorResult(req, phelixerr.Wrap(phelixerr.CodeUnavailable,
				"autoscale command completed but its durable result could not be recorded", err)))
		} else if monitorStream.sendCommandResult(result) == nil {
			if err := autoscaleResults.markDelivered(req.GetRequestId()); err != nil {
				logs.ErrorFile("grpc", "[gRPC Autoscale] failed to mark result delivered: %v", err)
			}
		}
	}()
}

// executeAutoscaleCommand runs the registered handler with panic isolation.
func executeAutoscaleCommand(req *pb.MonitorCommandRequest) (result *pb.MonitorCommandResult) {
	handler := getAutoscaleHandler()
	if handler == nil {
		return finalizeAutoscaleResult(req, commandErrorResult(req,
			phelixerr.New(phelixerr.CodeUnimplemented, "autoscale command not available: no autoscale handler registered")))
	}
	defer func() {
		if rec := recover(); rec != nil {
			logs.ErrorFile("grpc", "[gRPC Autoscale] handler panicked: %v", rec)
			result = finalizeAutoscaleResult(req, commandErrorResult(req,
				phelixerr.Newf(phelixerr.CodeUnknown, "autoscale command panicked: %v", rec)))
		}
	}()
	result = handler(req)
	if result == nil {
		result = commandErrorResult(req, phelixerr.New(phelixerr.CodeUnknown, "autoscale handler returned no result"))
	}
	return finalizeAutoscaleResult(req, result)
}

// finalizeAutoscaleResult stamps the transport-level identity fields the
// handler does not own, defaults the error code on error results, and redacts.
func finalizeAutoscaleResult(req *pb.MonitorCommandRequest, result *pb.MonitorCommandResult) *pb.MonitorCommandResult {
	result.RequestId = req.GetRequestId()
	result.Command = req.GetType()
	result.AppName = req.GetAppName()
	result.Timestamp = time.Now().UnixMilli()
	if result.Status == "error" {
		if result.ErrorCode == "" {
			result.ErrorCode = string(phelixerr.CodeUnknown)
		}
		result.Error = phelixerr.Redact(result.Error)
	}
	return result
}

// replayPendingAutoscaleResults re-sends autoscale command results that were
// durably recorded but never confirmed delivered. Called on every
// MonitorStream (re)connection.
func (c *Client) replayPendingAutoscaleResults() {
	results, err := autoscaleResults.pendingResults()
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC Autoscale] cannot load pending autoscale results: %v", err)
		return
	}
	for _, result := range results {
		if err := monitorStream.sendCommandResult(result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Autoscale] failed to replay autoscale result request_id=%s: %v", result.GetRequestId(), err)
			return
		}
		if err := autoscaleResults.markDelivered(result.GetRequestId()); err != nil {
			logs.ErrorFile("grpc", "[gRPC Autoscale] failed to mark autoscale result delivered request_id=%s: %v", result.GetRequestId(), err)
			return
		}
	}
}
