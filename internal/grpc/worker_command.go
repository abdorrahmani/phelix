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

// Remote worker lifecycle command dispatch. worker_scale arrives over the
// MonitorStream (MonitorCommandRequest with type worker_scale and the
// existing replicas option) and runs the SAME deploy.WorkerScale the local
// `phelix scale` command uses, under the same per-app replica coordinator
// lock: there is no second scaling implementation. The result carries the
// before/after replica counts in additive MonitorCommandResult fields.
//
// worker_scale is a mutation and is idempotent by request_id through a
// durable ledger, exactly like the webhook family: same request_id + same
// payload replays the stored result, same request_id + different payload is
// ALREADY_EXISTS, and a request in progress answers UNAVAILABLE.
//
// The cmd package registers the real implementation (SetWorkerHandler).

// Worker command types (MonitorCommandRequest.type).
const (
	WorkerCommandScale = "worker_scale"
)

// workerLedgerFile is the durable idempotency ledger for remote worker
// mutations, kept separate from the other ledgers.
const workerLedgerFile = "remote-worker-ledger.json"

// The worker dispatch compiled into this build declares its capability: the
// backend may gate worker_* commands on it.
func init() { RegisterCapability(CapabilityWorkerLifecycle) }

// WorkerHandlerFunc executes one worker command and returns its fully
// populated result.
type WorkerHandlerFunc func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult

var (
	workerHandlerMu sync.RWMutex
	workerHandler   WorkerHandlerFunc
)

// SetWorkerHandler registers the remote worker implementation. The daemon
// wiring calls it before the client starts.
func SetWorkerHandler(h WorkerHandlerFunc) {
	workerHandlerMu.Lock()
	defer workerHandlerMu.Unlock()
	workerHandler = h
}

func getWorkerHandler() WorkerHandlerFunc {
	workerHandlerMu.RLock()
	defer workerHandlerMu.RUnlock()
	return workerHandler
}

var workerResults = newRollbackLedger(filepath.Join(server.DataDir(), workerLedgerFile), rollbackLedgerCapacity, "worker",
	"the replica count may or may not have been applied — reconcile with the app's deployment snapshot")

// InitializeWorkerLedger loads the durable remote-worker idempotency ledger.
// Monitor startup must call it before accepting worker commands.
func InitializeWorkerLedger() error {
	return workerResults.initializeAt(filepath.Join(server.DataDir(), workerLedgerFile))
}

// pendingWorkerWG tracks in-flight mutating worker commands for shutdown.
var pendingWorkerWG sync.WaitGroup

// WaitPendingWorkerCommands waits for in-flight worker commands to settle,
// bounded by timeout.
func WaitPendingWorkerCommands(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		pendingWorkerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logs.WarningFile("grpc", "[gRPC Worker] shutdown drain timed out after %s", timeout)
	}
}

// isWorkerCommand reports whether a command type is a worker command.
func isWorkerCommand(t string) bool {
	return t == WorkerCommandScale
}

// validateWorkerScaleCommand checks the transport-level shape of a worker
// command before anything executes.
func validateWorkerScaleCommand(req *pb.MonitorCommandRequest) error {
	if req.GetRequestId() == "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires a request_id", req.GetType())
	}
	if req.GetAppName() == "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires an app_name (the app's stable ID or its name)", req.GetType())
	}
	if req.GetStrategy() != "" || req.GetReason() != "" || req.GetVerifyDurationMs() != 0 ||
		req.GetDryRun() || req.GetTarget() != "" || req.GetMatrix() != nil || req.GetWebhook() != nil {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s command must not carry strategy, reason, verify_duration_ms, dry_run, target, matrix or webhook options", req.GetType())
	}
	if req.GetReplicas() < 1 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires replicas >= 1, got %d", req.GetType(), req.GetReplicas())
	}
	return nil
}

// handleWorkerCommand dispatches one worker command. It is validated and
// ledger-begun synchronously, then executed on a background goroutine; the
// terminal result is persisted in the ledger BEFORE the send attempt.
func (c *Client) handleWorkerCommand(req *pb.MonitorCommandRequest, ledger *rollbackLedger) {
	if err := validateWorkerScaleCommand(req); err != nil {
		_ = sendCommandResultLogged(finalizeWorkerResult(req, commandErrorResult(req, err)))
		return
	}

	outcome, replay, err := ledger.begin(req)
	if err != nil {
		_ = sendCommandResultLogged(finalizeWorkerResult(req, commandErrorResult(req, err)))
		return
	}
	if outcome == rollbackBeginReplay {
		if sendCommandResultLogged(replay) == nil {
			if err := ledger.markDelivered(req.GetRequestId()); err != nil {
				logs.ErrorFile("grpc", "[gRPC Worker] failed to mark replay delivered: %v", err)
			}
		}
		return
	}

	pendingWorkerWG.Add(1)
	go func() {
		defer pendingWorkerWG.Done()
		result := executeWorkerCommand(req)
		if err := ledger.complete(req.GetRequestId(), result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Worker] result persistence failed: %v", err)
			result = finalizeWorkerResult(req, commandErrorResult(req, phelixerr.Wrap(phelixerr.CodeUnavailable,
				"worker command completed but its durable result could not be recorded", err)))
		} else if monitorStream.sendCommandResult(result) == nil {
			if err := ledger.markDelivered(req.GetRequestId()); err != nil {
				logs.ErrorFile("grpc", "[gRPC Worker] failed to mark result delivered: %v", err)
			}
		}
	}()
}

// executeWorkerCommand runs the registered handler with panic isolation.
func executeWorkerCommand(req *pb.MonitorCommandRequest) (result *pb.MonitorCommandResult) {
	handler := getWorkerHandler()
	if handler == nil {
		return finalizeWorkerResult(req, commandErrorResult(req,
			phelixerr.New(phelixerr.CodeUnimplemented, "worker command not available: no worker handler registered")))
	}
	defer func() {
		if rec := recover(); rec != nil {
			logs.ErrorFile("grpc", "[gRPC Worker] handler panicked: %v", rec)
			result = finalizeWorkerResult(req, commandErrorResult(req,
				phelixerr.Newf(phelixerr.CodeUnknown, "worker command panicked: %v", rec)))
		}
	}()
	result = handler(req)
	if result == nil {
		result = commandErrorResult(req, phelixerr.New(phelixerr.CodeUnknown, "worker handler returned no result"))
	}
	return finalizeWorkerResult(req, result)
}

// finalizeWorkerResult stamps the transport-level identity fields the handler
// does not own, defaults the error code on error results, and redacts.
func finalizeWorkerResult(req *pb.MonitorCommandRequest, result *pb.MonitorCommandResult) *pb.MonitorCommandResult {
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

// replayPendingWorkerResults re-sends worker command results that were
// durably recorded but never confirmed delivered. Called on every
// MonitorStream (re)connection.
func (c *Client) replayPendingWorkerResults(ledger *rollbackLedger) {
	results, err := ledger.pendingResults()
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC Worker] cannot load pending worker results: %v", err)
		return
	}
	for _, result := range results {
		if err := monitorStream.sendCommandResult(result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Worker] failed to replay worker result request_id=%s: %v", result.GetRequestId(), err)
			return
		}
		if err := ledger.markDelivered(result.GetRequestId()); err != nil {
			logs.ErrorFile("grpc", "[gRPC Worker] failed to mark worker result delivered request_id=%s: %v", result.GetRequestId(), err)
			return
		}
	}
}
