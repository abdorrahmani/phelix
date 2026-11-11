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

// Remote job management command dispatch. job_* commands arrive over the
// MonitorStream like every remote command and expose the SAME job engine the
// local CLI uses (configured phelix.yaml jobs, durable executions and logs).
// There is no second scheduler or executor.
//
//   - job_list, job_history and job_logs are read-only and answered without
//     the ledger;
//   - job_run is a mutation and is idempotent by request_id through a durable
//     ledger: same request_id + same payload replays the stored result, same
//     request_id + different payload is ALREADY_EXISTS, and a request in
//     progress answers UNAVAILABLE;
//   - terminal execution transitions travel separately as
//     MonitorEvent.job_event observations.
//
// The cmd package registers the real implementation (SetJobHandler).

// Job command types (MonitorCommandRequest.type).
const (
	JobCommandList    = "job_list"
	JobCommandRun     = "job_run"
	JobCommandHistory = "job_history"
	JobCommandLogs    = "job_logs"
)

// jobLedgerFile is the durable idempotency ledger for remote job mutations,
// kept separate from the other feature ledgers.
const jobLedgerFile = "remote-job-ledger.json"

// jobCommandSettleTimeout bounds how long daemon shutdown waits for in-flight
// job commands (acceptance, ledger persistence and the result send).
const jobCommandSettleTimeout = 10 * time.Second

// JobCommandSettleTimeout exposes the shutdown drain bound for the daemon.
func JobCommandSettleTimeout() time.Duration {
	return jobCommandSettleTimeout
}

// The job remote-management dispatch compiled into this build declares its
// capability: the backend may gate job_* commands on it.
func init() { RegisterCapability(CapabilityJobManagement) }

// JobHandlerFunc executes one job command and returns its fully populated
// result. The dispatcher stamps RequestId, Command, AppName and Timestamp.
type JobHandlerFunc func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult

// JobAuthorizerFunc gates the mutating job_run command through the host
// authorization boundary. It runs before any ledger entry or execution.
type JobAuthorizerFunc func(req *pb.MonitorCommandRequest) error

var (
	jobHandlerMu sync.RWMutex
	jobHandler   JobHandlerFunc

	jobAuthorizerMu sync.RWMutex
	jobAuthorizer   JobAuthorizerFunc
)

// SetJobHandler registers the remote job implementation. The daemon wiring
// calls it before the client starts.
func SetJobHandler(h JobHandlerFunc) {
	jobHandlerMu.Lock()
	defer jobHandlerMu.Unlock()
	jobHandler = h
}

func getJobHandler() JobHandlerFunc {
	jobHandlerMu.RLock()
	defer jobHandlerMu.RUnlock()
	return jobHandler
}

// SetJobAuthorizer registers the job_run authorization gate.
func SetJobAuthorizer(a JobAuthorizerFunc) {
	jobAuthorizerMu.Lock()
	defer jobAuthorizerMu.Unlock()
	jobAuthorizer = a
}

func getJobAuthorizer() JobAuthorizerFunc {
	jobAuthorizerMu.RLock()
	defer jobAuthorizerMu.RUnlock()
	return jobAuthorizer
}

// jobResults is the durable idempotency ledger for remote job mutations.
var jobResults = newRollbackLedger(filepath.Join(server.DataDir(), jobLedgerFile), rollbackLedgerCapacity, "job",
	"the job may or may not have been started; reconcile with a job_history command")

// InitializeJobLedger loads the durable remote-job idempotency ledger.
// Monitor startup must call it before accepting job commands. A corrupt or
// unreadable ledger fails closed (job_run answers UNAVAILABLE instead of
// executing without idempotency).
func InitializeJobLedger() error {
	return jobResults.initializeAt(filepath.Join(server.DataDir(), jobLedgerFile))
}

// JobLedgerPath exposes the effective job ledger location for diagnostics.
func JobLedgerPath() string {
	jobResults.mu.Lock()
	defer jobResults.mu.Unlock()
	return jobResults.path
}

// pendingJobWG tracks in-flight job commands for shutdown.
var pendingJobWG sync.WaitGroup

// WaitPendingJobCommands waits for in-flight job commands to settle, bounded
// by timeout.
func WaitPendingJobCommands(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		pendingJobWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logs.WarningFile("grpc", "[gRPC Job] shutdown drain timed out after %s", timeout)
	}
}

// Job query bounds, matching the job engine's retention window.
const (
	JobHistoryLimitDefault = 20
	JobHistoryLimitMax     = 100
	JobLogBytesDefault     = 64 * 1024
	JobLogBytesMax         = 1024 * 1024
)

// isJobCommand reports whether a command type is a job command.
func isJobCommand(t string) bool {
	switch t {
	case JobCommandList, JobCommandRun, JobCommandHistory, JobCommandLogs:
		return true
	}
	return false
}

// isMutatingJobCommand reports whether the command changes state. Only
// job_run starts an execution; the other three are read-only.
func isMutatingJobCommand(t string) bool {
	return t == JobCommandRun
}

// validateJobCommand checks the transport-level shape of a job command:
// correlation, foreign options, and per-type option rules. Semantic
// validation (app is a real job, execution belongs to it) happens in the
// handler against the same registry and store the CLI uses.
func validateJobCommand(req *pb.MonitorCommandRequest) error {
	if req.GetRequestId() == "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires a request_id", req.GetType())
	}
	if req.GetAppName() == "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires an app_name (the app's stable ID or its name)", req.GetType())
	}
	if req.GetStrategy() != "" || req.GetReplicas() != 0 || req.GetReason() != "" ||
		req.GetVerifyDurationMs() != 0 || req.GetDryRun() || req.GetTarget() != "" ||
		req.GetMatrix() != nil || req.GetWebhook() != nil || req.GetLimit() != 0 ||
		req.GetReplica() != "" || req.GetSecurity() != nil {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s command must not carry rollback/rebuild/matrix/webhook/security options", req.GetType())
	}
	if !isJobCommand(req.GetType()) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unknown job command type %q", req.GetType())
	}

	opts := req.GetJob()
	if opts == nil {
		if req.GetType() == JobCommandLogs {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "job_logs requires job.execution_id")
		}
		return nil
	}
	if len(opts.GetJobName()) > 200 {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "job_name is too long (max 200 characters)")
	}
	if opts.GetLimit() < 0 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "job limit must be 0 (default %d) or positive, got %d", JobHistoryLimitDefault, opts.GetLimit())
	}
	if opts.GetLimit() > JobHistoryLimitMax {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "job limit must not exceed %d, got %d", JobHistoryLimitMax, opts.GetLimit())
	}
	if opts.GetOffset() < 0 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "job log offset must not be negative, got %d", opts.GetOffset())
	}
	if opts.GetMaxBytes() < 0 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "job log max_bytes must not be negative, got %d", opts.GetMaxBytes())
	}
	if opts.GetMaxBytes() > JobLogBytesMax {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "job log max_bytes must not exceed %d, got %d", JobLogBytesMax, opts.GetMaxBytes())
	}

	switch req.GetType() {
	case JobCommandList:
		if opts.GetLimit() != 0 || opts.GetExecutionId() != "" || opts.GetOffset() != 0 || opts.GetMaxBytes() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "job_list accepts only the optional job.job_name option")
		}
	case JobCommandRun:
		if opts.GetLimit() != 0 || opts.GetExecutionId() != "" || opts.GetOffset() != 0 || opts.GetMaxBytes() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "job_run accepts only the optional job.job_name option")
		}
	case JobCommandHistory:
		if opts.GetOffset() != 0 || opts.GetMaxBytes() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "job_history accepts only job.limit and job.execution_id")
		}
	case JobCommandLogs:
		if opts.GetExecutionId() == "" {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "job_logs requires job.execution_id")
		}
		if opts.GetJobName() != "" || opts.GetLimit() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "job_logs accepts only job.execution_id, job.offset and job.max_bytes")
		}
	}
	return nil
}

// handleJobCommand dispatches one job command. Mutations are validated,
// authorized and ledger-begun synchronously, then executed on a background
// goroutine; their terminal result is persisted in the ledger BEFORE the send
// attempt and replayed on the next stream connection when delivery cannot be
// confirmed. Queries are answered synchronously.
func (c *Client) handleJobCommand(req *pb.MonitorCommandRequest, ledger *rollbackLedger) {
	if err := validateJobCommand(req); err != nil {
		_ = sendCommandResultLogged(finalizeJobResult(req, commandErrorResult(req, err)))
		return
	}

	if !isMutatingJobCommand(req.GetType()) {
		_ = sendCommandResultLogged(executeJobCommand(req))
		return
	}

	if authorize := getJobAuthorizer(); authorize != nil {
		if err := authorize(req); err != nil {
			_ = sendCommandResultLogged(finalizeJobResult(req, commandErrorResult(req, err)))
			return
		}
	}

	outcome, replay, err := ledger.begin(req)
	if err != nil {
		_ = sendCommandResultLogged(finalizeJobResult(req, commandErrorResult(req, err)))
		return
	}
	if outcome == rollbackBeginReplay {
		if sendCommandResultLogged(replay) == nil {
			if err := ledger.markDelivered(req.GetRequestId()); err != nil {
				logs.ErrorFile("grpc", "[gRPC Job] failed to mark replay delivered: %v", err)
			}
		}
		return
	}

	pendingJobWG.Add(1)
	go func() {
		defer pendingJobWG.Done()
		result := executeJobCommand(req)
		if err := ledger.complete(req.GetRequestId(), result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Job] result persistence failed: %v", err)
			result = finalizeJobResult(req, commandErrorResult(req, phelixerr.Wrap(phelixerr.CodeUnavailable,
				"job command completed but its durable result could not be recorded", err)))
		} else if monitorStream.sendCommandResult(result) == nil {
			if err := ledger.markDelivered(req.GetRequestId()); err != nil {
				logs.ErrorFile("grpc", "[gRPC Job] failed to mark result delivered: %v", err)
			}
		}
		// A failed send leaves the entry pending: the next MonitorStream
		// connection replays it (replayPendingJobResults).
	}()
}

// executeJobCommand runs the registered handler with panic isolation.
func executeJobCommand(req *pb.MonitorCommandRequest) (result *pb.MonitorCommandResult) {
	handler := getJobHandler()
	if handler == nil {
		return finalizeJobResult(req, commandErrorResult(req,
			phelixerr.New(phelixerr.CodeUnimplemented, "job command not available: no job handler registered")))
	}
	defer func() {
		if rec := recover(); rec != nil {
			logs.ErrorFile("grpc", "[gRPC Job] handler panicked: %v", rec)
			result = finalizeJobResult(req, commandErrorResult(req,
				phelixerr.Newf(phelixerr.CodeUnknown, "job command panicked: %v", rec)))
		}
	}()
	result = handler(req)
	if result == nil {
		result = commandErrorResult(req, phelixerr.New(phelixerr.CodeUnknown, "job handler returned no result"))
	}
	return finalizeJobResult(req, result)
}

// finalizeJobResult stamps the transport-level identity fields, defaults the
// error code on error results, and redacts the error text.
func finalizeJobResult(req *pb.MonitorCommandRequest, result *pb.MonitorCommandResult) *pb.MonitorCommandResult {
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

// SendJobLifecycleEvent delivers one job lifecycle observation through the
// existing MonitorStream. Delivery is best-effort: a disconnected backend
// re-reads durable history, so a dropped event never loses state.
func SendJobLifecycleEvent(event *pb.JobLifecycleEvent) {
	if event == nil {
		return
	}
	if event.GetServerId() == "" {
		event.ServerId = server.GetServerID()
	}
	if err := monitorStream.send(&pb.MonitorEvent{
		ServerId:  event.GetServerId(),
		Timestamp: event.GetTimestamp(),
		Payload:   &pb.MonitorEvent_JobEvent{JobEvent: event},
	}); err != nil {
		logs.WarningFile("grpc", "[gRPC Job] failed to send lifecycle event execution_id=%s phase=%s: %v",
			event.GetExecutionId(), event.GetPhase(), err)
	}
}

// replayPendingJobResults re-sends job command results that were durably
// recorded but never confirmed delivered. Called on every MonitorStream
// (re)connection.
func (c *Client) replayPendingJobResults(ledger *rollbackLedger) {
	results, err := ledger.pendingResults()
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC Job] cannot load pending job results: %v", err)
		return
	}
	for _, result := range results {
		if err := monitorStream.sendCommandResult(result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Job] failed to replay job result request_id=%s: %v", result.GetRequestId(), err)
			return
		}
		if err := ledger.markDelivered(result.GetRequestId()); err != nil {
			logs.ErrorFile("grpc", "[gRPC Job] failed to mark job result delivered request_id=%s: %v", result.GetRequestId(), err)
			return
		}
	}
}
