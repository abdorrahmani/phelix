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

// Remote Build Matrix command dispatch. Matrix commands arrive over the
// MonitorStream like every remote command (MonitorCommandRequest with a
// matrix_* type and the MatrixOptions payload), but unlike lifecycle commands
// they are NOT executed through the monitor command executor:
//
//   - they are long-running (minutes), so mutating commands execute in a
//     background goroutine — the receive loop and the metrics tick keep
//     running while a matrix build works;
//   - they are idempotent by request_id through a durable ledger, exactly
//     like remote rollbacks: same request_id + same payload replays the stored
//     result, same request_id + different payload is ALREADY_EXISTS, and a
//     request in progress answers UNAVAILABLE;
//   - their results carry structured matrix state (run/artifacts/checksums)
//     in additive MonitorCommandResult fields, so the backend never parses
//     human-readable output.
//
// The cmd package registers the real implementation (SetMatrixHandler); it
// drives the SAME matrix engine the local CLI commands use.

// Matrix command types (MonitorCommandRequest.type).
const (
	MatrixCommandBuild     = "matrix_build"
	MatrixCommandDockerize = "matrix_dockerize"
	MatrixCommandResume    = "matrix_resume"
	MatrixCommandRetry     = "matrix_retry"
	MatrixCommandStatus    = "matrix_status"
	MatrixCommandList      = "matrix_list"
)

// matrixLedgerFile is the durable idempotency ledger for remote matrix
// commands. Kept separate from the rollback ledger so a corrupt or exhausted
// matrix ledger cannot take remote rollback idempotency down with it.
const matrixLedgerFile = "remote-matrix-ledger.json"

// matrixCommandSettleTimeout bounds how long daemon shutdown waits for
// in-flight matrix commands (builds are canceled through their context first;
// this covers finalization, ledger persistence and the result send).
const matrixCommandSettleTimeout = 15 * time.Second

// MatrixCommandSettleTimeout exposes the shutdown drain bound for the daemon
// wiring.
func MatrixCommandSettleTimeout() time.Duration {
	return matrixCommandSettleTimeout
}

// The matrix command implementation compiled into this build declares its
// capability: this module (the dispatch the MonitorStream routes matrix_*
// commands to) existing IS the support — the backend may gate matrix
// commands on it.
func init() { RegisterCapability(CapabilityBuildMatrix) }

// MatrixHandlerFunc executes one matrix command and returns its fully
// populated result. The handler sets Status ("success"|"error"), Error and
// ErrorCode on failure, and the matrix_* payload fields appropriate for the
// command; the dispatcher stamps RequestId, Command, AppName and Timestamp.
// Handlers execute on the dispatch goroutine and may block for the whole
// build — the dispatcher owns asynchrony.
type MatrixHandlerFunc func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult

var (
	matrixHandlerMu sync.RWMutex
	matrixHandler   MatrixHandlerFunc
)

// SetMatrixHandler registers the remote matrix implementation. The daemon
// wiring calls it before the client starts, so no command can arrive in the
// startup race window and be answered UNIMPLEMENTED.
func SetMatrixHandler(h MatrixHandlerFunc) {
	matrixHandlerMu.Lock()
	defer matrixHandlerMu.Unlock()
	matrixHandler = h
}

func getMatrixHandler() MatrixHandlerFunc {
	matrixHandlerMu.RLock()
	defer matrixHandlerMu.RUnlock()
	return matrixHandler
}

// MatrixAuthorizeFunc authorizes one mutating matrix command before it is
// recorded in the idempotency ledger or executed. It returns a non-nil error
// (a structured phelixerr) exactly when the command must NOT run. The cmd
// package registers it (SetMatrixAuthorizer) so the grpc dispatch gates a
// backend-issued matrix mutation through the same execution-authorization
// boundary a remote rebuild/rollback crosses — a daemon-side deploy-class
// mutation must not be the one that bypasses it.
type MatrixAuthorizeFunc func(req *pb.MonitorCommandRequest) error

var (
	matrixAuthorizerMu sync.RWMutex
	matrixAuthorizer   MatrixAuthorizeFunc
)

// SetMatrixAuthorizer registers the remote matrix authorization gate. A nil
// authorizer (the default, and in unit tests that do not wire one) means the
// dispatch does not gate — the same posture as a host with no authorization
// policy configured.
func SetMatrixAuthorizer(f MatrixAuthorizeFunc) {
	matrixAuthorizerMu.Lock()
	defer matrixAuthorizerMu.Unlock()
	matrixAuthorizer = f
}

func getMatrixAuthorizer() MatrixAuthorizeFunc {
	matrixAuthorizerMu.RLock()
	defer matrixAuthorizerMu.RUnlock()
	return matrixAuthorizer
}

// maxConcurrentMatrixExecutions bounds how many mutating matrix commands may
// execute at once in the daemon. Each one is a full multi-combination build
// (itself running up to its own concurrency of parallel compilations), so the
// local one-build-per-process model has no analogue here: without a bound a
// backend — or a retry storm — could launch unbounded concurrent builds and
// exhaust host CPU/memory/disk. Beyond the limit a new mutating command is
// answered UNAVAILABLE (consuming no idempotency key) and the backend retries
// once a slot frees.
//
// ponytail: a fixed 2-slot semaphore (never blocks the receive loop — the
// acquire is non-blocking). Raise it, or replace it with a real queue, if a
// host should run more concurrent matrix builds.
const maxConcurrentMatrixExecutions = 2

// matrixExecSlots is the concurrency semaphore for mutating matrix executions.
// A var so a test can substitute a differently-sized channel; handleMatrixCommand
// captures it locally per command so a swap never crosses acquire/release.
var matrixExecSlots = make(chan struct{}, maxConcurrentMatrixExecutions)

// matrixResults is the durable idempotency ledger for remote matrix commands.
var matrixResults = newRollbackLedger(filepath.Join(server.DataDir(), matrixLedgerFile), rollbackLedgerCapacity, "matrix",
	"any matrix run the command created is persisted with its per-combination state — inspect it with a matrix_status command or continue it with matrix_resume")

// InitializeMatrixLedger loads the durable remote-matrix idempotency ledger.
// Monitor startup must call it before accepting matrix commands; a corrupt or
// unreadable ledger fails closed (matrix commands answer UNAVAILABLE instead
// of executing without idempotency). See InitializeRollbackLedger.
func InitializeMatrixLedger() error {
	return matrixResults.initializeAt(filepath.Join(server.DataDir(), matrixLedgerFile))
}

// MatrixLedgerPath exposes the effective matrix ledger location for startup
// diagnostics and integration tests.
func MatrixLedgerPath() string {
	matrixResults.mu.Lock()
	defer matrixResults.mu.Unlock()
	return matrixResults.path
}

// pendingMatrixWG tracks in-flight mutating matrix commands (execution +
// result persistence + send attempt) so daemon shutdown can drain them.
var pendingMatrixWG sync.WaitGroup

// WaitPendingMatrixCommands waits for in-flight matrix commands to settle,
// bounded by timeout. The daemon cancels build contexts first (see the cmd
// wiring); this drain covers finalization and the durable result write.
func WaitPendingMatrixCommands(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		pendingMatrixWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logs.WarningFile("grpc", "[gRPC Matrix] shutdown drain timed out after %s", timeout)
	}
}

// isMatrixCommand reports whether a command type is a matrix command.
func isMatrixCommand(t string) bool {
	switch t {
	case MatrixCommandBuild, MatrixCommandDockerize, MatrixCommandResume,
		MatrixCommandRetry, MatrixCommandStatus, MatrixCommandList:
		return true
	}
	return false
}

// isMutatingMatrixCommand reports whether the command changes state (and is
// therefore ledger-guarded). matrix_status and matrix_list are read-only
// queries: they are answered directly and never recorded in the ledger — a
// query has no execution to make idempotent, and the backend can re-ask at
// will.
func isMutatingMatrixCommand(t string) bool {
	switch t {
	case MatrixCommandBuild, MatrixCommandDockerize, MatrixCommandResume, MatrixCommandRetry:
		return true
	}
	return false
}

// matrixDryRunnable reports whether dry_run is meaningful for the command.
// Retry has no local dry-run flag, and queries are already read-only —
// dry_run on them is rejected instead of silently ignored.
func matrixDryRunnable(t string) bool {
	switch t {
	case MatrixCommandBuild, MatrixCommandDockerize, MatrixCommandResume:
		return true
	}
	return false
}

// validateMatrixCommand checks the transport-level shape of a matrix command
// before anything executes: correlation, foreign option fields (matrix
// commands must not carry rollback/rebuild options), per-type target rules
// and numeric option ranges. Semantic validation (versions, platforms, run
// existence) happens in the handler through the same matrix.Resolve/Expand
// path the local CLI uses — there is no second validation rule set.
func validateMatrixCommand(req *pb.MonitorCommandRequest) error {
	if req.GetRequestId() == "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires a request_id", req.GetType())
	}
	if req.GetStrategy() != "" || req.GetReplicas() != 0 || req.GetReason() != "" || req.GetVerifyDurationMs() != 0 || req.GetCanaryPercent() != 0 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s command must not carry rollback/rebuild options (strategy, replicas, reason, verify_duration_ms, canary_percent)", req.GetType())
	}
	if req.GetWebhook() != nil {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s command must not carry webhook options (webhook)", req.GetType())
	}
	if req.GetDryRun() && !matrixDryRunnable(req.GetType()) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "dry_run is not supported for %s commands", req.GetType())
	}

	switch req.GetType() {
	case MatrixCommandBuild, MatrixCommandDockerize:
		if req.GetAppName() == "" {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires an app_name", req.GetType())
		}
		if req.GetTarget() != "" {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command must not carry a target", req.GetType())
		}
	case MatrixCommandResume, MatrixCommandRetry:
		if req.GetTarget() == "" {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"%s command requires a target: a matrix run ID (mx_YYYYMMDD_xxxx) or \"latest\" (resume only)", req.GetType())
		}
		if req.GetType() == MatrixCommandRetry && req.GetTarget() == "latest" {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "matrix_retry requires an explicit source run ID — \"latest\" is only valid for matrix_resume")
		}
	case MatrixCommandStatus, MatrixCommandList:
		// target optional for status (empty = active run); rejected for list.
		if req.GetType() == MatrixCommandList && req.GetTarget() != "" {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "matrix_list command must not carry a target")
		}
	default:
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unknown matrix command type %q", req.GetType())
	}

	opts := req.GetMatrix()
	if opts == nil {
		return nil
	}
	if opts.GetConcurrency() < 0 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix concurrency must be 0 (default) or positive, got %d", opts.GetConcurrency())
	}
	if opts.GetRetries() < 0 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix retries must be 0 or positive, got %d", opts.GetRetries())
	}
	if opts.GetListLimit() < 0 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix list_limit must be 0 (default 20) or positive, got %d", opts.GetListLimit())
	}
	return nil
}

// handleMatrixCommand dispatches one matrix command. A mutating command is
// validated, authorized, admitted to the bounded execution semaphore and
// ledger-begun synchronously, then executed on a background goroutine; its
// terminal result is persisted in the ledger BEFORE the send attempt and
// replayed on the next stream connection when the send could not be confirmed.
// Read-only commands (status/list) and dry-run previews are answered
// synchronously without a ledger entry, an authorization gate or an execution
// slot — they mutate nothing. Authorization and the semaphore are both checked
// BEFORE ledger.begin, so a refusal or a capacity rejection consumes no
// idempotency key.
//
// Metrics are deliberately NOT paused during matrix commands: a matrix build
// runs for minutes, and monitoring the host while it works is the point.
func (c *Client) handleMatrixCommand(req *pb.MonitorCommandRequest, ledger *rollbackLedger) {
	if err := validateMatrixCommand(req); err != nil {
		_ = sendCommandResultLogged(finalizeMatrixResult(req, commandErrorResult(req, err)))
		return
	}

	// Read-only queries (status/list) and dry-run previews mutate nothing —
	// including idempotency state (F2): they take no ledger entry, no
	// authorization gate and no execution slot, and are answered synchronously
	// on this goroutine (a preview is cheap; a bounded query scan — see
	// matrix.RecentRuns — keeps it so).
	if !isMutatingMatrixCommand(req.GetType()) || req.GetDryRun() {
		_ = sendCommandResultLogged(executeMatrixCommand(req))
		return
	}

	// Execution-authorization boundary (F3): a backend-issued matrix mutation
	// runs in the daemon, so it needs the same gate a remote rebuild/rollback
	// crosses. Gated BEFORE the ledger so a refusal spends no idempotency key —
	// a corrected retry under the same request_id then executes. status/list
	// and dry-run above are read-only and stay ungated.
	if authorize := getMatrixAuthorizer(); authorize != nil {
		if err := authorize(req); err != nil {
			_ = sendCommandResultLogged(finalizeMatrixResult(req, commandErrorResult(req, err)))
			return
		}
	}

	// Concurrency bound (F6): non-blocking acquire BEFORE the ledger so a
	// capacity rejection also spends no idempotency key. The slot is captured
	// locally so a test that swaps matrixExecSlots cannot cross acquire/release.
	slots := matrixExecSlots
	select {
	case slots <- struct{}{}:
	default:
		_ = sendCommandResultLogged(finalizeMatrixResult(req, commandErrorResult(req, phelixerr.Newf(phelixerr.CodeUnavailable,
			"agent is already running %d matrix builds; retry once one finishes", cap(slots)))))
		return
	}

	outcome, replay, err := ledger.begin(req)
	if err != nil {
		<-slots
		_ = sendCommandResultLogged(finalizeMatrixResult(req, commandErrorResult(req, err)))
		return
	}
	if outcome == rollbackBeginReplay {
		<-slots
		if sendCommandResultLogged(replay) == nil {
			if err := ledger.markDelivered(req.GetRequestId()); err != nil {
				logs.ErrorFile("grpc", "[gRPC Matrix] failed to mark replay delivered: %v", err)
			}
		}
		return
	}

	pendingMatrixWG.Add(1)
	go func() {
		defer func() { <-slots }()
		defer pendingMatrixWG.Done()
		result := executeMatrixCommand(req)
		if err := ledger.complete(req.GetRequestId(), result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Matrix] result persistence failed: %v", err)
			result = finalizeMatrixResult(req, commandErrorResult(req, phelixerr.Wrap(phelixerr.CodeUnavailable,
				"matrix command completed but its durable result could not be recorded", err)))
		} else if monitorStream.sendCommandResult(result) == nil {
			if err := ledger.markDelivered(req.GetRequestId()); err != nil {
				logs.ErrorFile("grpc", "[gRPC Matrix] failed to mark result delivered: %v", err)
			}
		}
		// A failed send leaves the entry pending: the next MonitorStream
		// connection replays it (replayPendingMatrixResults).
	}()
}

// executeMatrixCommand runs the registered handler with panic isolation — a
// panic in a background matrix execution must convert to a terminal error
// result, never take the daemon down. The result always carries the command's
// identity fields and a structured error code (UNKNOWN for unclassified
// failures — the same convention as lifecycle commands).
func executeMatrixCommand(req *pb.MonitorCommandRequest) (result *pb.MonitorCommandResult) {
	handler := getMatrixHandler()
	if handler == nil {
		return finalizeMatrixResult(req, commandErrorResult(req,
			phelixerr.New(phelixerr.CodeUnimplemented, "matrix command not available: no matrix handler registered")))
	}
	defer func() {
		if rec := recover(); rec != nil {
			logs.ErrorFile("grpc", "[gRPC Matrix] handler panicked: %v", rec)
			result = finalizeMatrixResult(req, commandErrorResult(req,
				phelixerr.Newf(phelixerr.CodeUnknown, "matrix command panicked: %v", rec)))
		}
	}()
	result = handler(req)
	if result == nil {
		result = commandErrorResult(req, phelixerr.New(phelixerr.CodeUnknown, "matrix handler returned no result"))
	}
	return finalizeMatrixResult(req, result)
}

// finalizeMatrixResult stamps the transport-level identity fields the handler
// does not own, defaults the error code on error results without one, and
// redacts the error text (run/combination errors are already redacted by the
// engine; this covers handler-level errors).
func finalizeMatrixResult(req *pb.MonitorCommandRequest, result *pb.MonitorCommandResult) *pb.MonitorCommandResult {
	result.RequestId = req.GetRequestId()
	result.Command = req.GetType()
	// Run-targeted commands (resume/retry/status by run ID) may omit app_name,
	// so this top-level field is then empty by design — the authoritative app
	// is matrix_run.app_name. Not a bug: the command simply did not name an app.
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

// replayPendingMatrixResults re-sends matrix command results that were
// durably recorded but never confirmed delivered (backend restart, dropped
// connection, agent restart). Called on every MonitorStream (re)connection.
func (c *Client) replayPendingMatrixResults(ledger *rollbackLedger) {
	results, err := ledger.pendingResults()
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC Matrix] cannot load pending matrix results: %v", err)
		return
	}
	for _, result := range results {
		if err := monitorStream.sendCommandResult(result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Matrix] failed to replay matrix result request_id=%s: %v", result.GetRequestId(), err)
			return
		}
		if err := ledger.markDelivered(result.GetRequestId()); err != nil {
			logs.ErrorFile("grpc", "[gRPC Matrix] failed to mark matrix result delivered request_id=%s: %v", result.GetRequestId(), err)
			return
		}
	}
}
