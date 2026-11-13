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

const (
	HooksCommandRun = "hooks_run"

	hookLedgerFile = "remote-hook-ledger.json"
)

const hookCommandSettleTimeout = 10 * time.Second

// HookCommandSettleTimeout exposes the shutdown drain bound for the daemon.
func HookCommandSettleTimeout() time.Duration {
	return hookCommandSettleTimeout
}

func init() { RegisterCapability(CapabilityLifecycleHooks) }

type HookHandlerFunc func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult

type HookAuthorizerFunc func(req *pb.MonitorCommandRequest) error

var (
	hookHandlerMu sync.RWMutex
	hookHandler   HookHandlerFunc

	hookAuthorizerMu sync.RWMutex
	hookAuthorizer   HookAuthorizerFunc
)

func SetHookHandler(h HookHandlerFunc) {
	hookHandlerMu.Lock()
	defer hookHandlerMu.Unlock()
	hookHandler = h
}

func getHookHandler() HookHandlerFunc {
	hookHandlerMu.RLock()
	defer hookHandlerMu.RUnlock()
	return hookHandler
}

func SetHookAuthorizer(a HookAuthorizerFunc) {
	hookAuthorizerMu.Lock()
	defer hookAuthorizerMu.Unlock()
	hookAuthorizer = a
}

func getHookAuthorizer() HookAuthorizerFunc {
	hookAuthorizerMu.RLock()
	defer hookAuthorizerMu.RUnlock()
	return hookAuthorizer
}

var hookResults = newRollbackLedger(filepath.Join(server.DataDir(), hookLedgerFile), rollbackLedgerCapacity, "hook",
	"the hook may or may not have run; reconcile with 'phelix hooks history'")

// InitializeHookLedger loads the durable remote-hook ledger.
func InitializeHookLedger() error {
	return hookResults.initializeAt(filepath.Join(server.DataDir(), hookLedgerFile))
}

func HookLedgerPath() string {
	hookResults.mu.Lock()
	defer hookResults.mu.Unlock()
	return hookResults.path
}

var pendingHookWG sync.WaitGroup

func WaitPendingHookCommands(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		pendingHookWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logs.WarningFile("grpc", "[gRPC Hook] shutdown drain timed out after %s", timeout)
	}
}

func isHookCommand(t string) bool {
	return t == HooksCommandRun
}

func validateHookCommand(req *pb.MonitorCommandRequest) error {
	if req.GetRequestId() == "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires a request_id", req.GetType())
	}
	if req.GetAppName() == "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires an app_name (the app's stable ID or its name)", req.GetType())
	}
	if req.GetType() != HooksCommandRun {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unknown hook command type %q", req.GetType())
	}
	if req.GetStrategy() != "" || req.GetReplicas() != 0 || req.GetReason() != "" ||
		req.GetVerifyDurationMs() != 0 || req.GetDryRun() || req.GetTarget() != "" ||
		req.GetMatrix() != nil || req.GetWebhook() != nil || req.GetLimit() != 0 ||
		req.GetReplica() != "" || req.GetSecurity() != nil || req.GetJob() != nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument,
			"hooks_run must not carry rollback/rebuild/matrix/webhook/security/job options")
	}
	opts := req.GetHooks()
	if opts == nil || opts.GetName() == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "hooks_run requires hooks.name")
	}
	return nil
}

func (c *Client) handleHookCommand(req *pb.MonitorCommandRequest, ledger *rollbackLedger) {
	if err := validateHookCommand(req); err != nil {
		_ = sendCommandResultLogged(finalizeHookResult(req, commandErrorResult(req, err)))
		return
	}
	if authorize := getHookAuthorizer(); authorize != nil {
		if err := authorize(req); err != nil {
			_ = sendCommandResultLogged(finalizeHookResult(req, commandErrorResult(req, err)))
			return
		}
	}

	outcome, replay, err := ledger.begin(req)
	if err != nil {
		_ = sendCommandResultLogged(finalizeHookResult(req, commandErrorResult(req, err)))
		return
	}
	if outcome == rollbackBeginReplay {
		if sendCommandResultLogged(replay) == nil {
			if err := ledger.markDelivered(req.GetRequestId()); err != nil {
				logs.ErrorFile("grpc", "[gRPC Hook] failed to mark replay delivered: %v", err)
			}
		}
		return
	}

	pendingHookWG.Add(1)
	go func() {
		defer pendingHookWG.Done()
		result := executeHookCommand(req)
		if err := ledger.complete(req.GetRequestId(), result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Hook] result persistence failed: %v", err)
			result = finalizeHookResult(req, commandErrorResult(req, phelixerr.Wrap(phelixerr.CodeUnavailable,
				"hook command completed but its durable result could not be recorded", err)))
		} else if monitorStream.sendCommandResult(result) == nil {
			if err := ledger.markDelivered(req.GetRequestId()); err != nil {
				logs.ErrorFile("grpc", "[gRPC Hook] failed to mark result delivered: %v", err)
			}
		}
	}()
}

func executeHookCommand(req *pb.MonitorCommandRequest) (result *pb.MonitorCommandResult) {
	handler := getHookHandler()
	if handler == nil {
		return finalizeHookResult(req, commandErrorResult(req,
			phelixerr.New(phelixerr.CodeUnimplemented, "hook command not available: no hook handler registered")))
	}
	defer func() {
		if rec := recover(); rec != nil {
			logs.ErrorFile("grpc", "[gRPC Hook] handler panicked: %v", rec)
			result = finalizeHookResult(req, commandErrorResult(req,
				phelixerr.Newf(phelixerr.CodeUnknown, "hook command panicked: %v", rec)))
		}
	}()
	result = handler(req)
	if result == nil {
		result = commandErrorResult(req, phelixerr.New(phelixerr.CodeUnknown, "hook command returned no result"))
	}
	return finalizeHookResult(req, result)
}

func finalizeHookResult(req *pb.MonitorCommandRequest, result *pb.MonitorCommandResult) *pb.MonitorCommandResult {
	if result == nil {
		result = &pb.MonitorCommandResult{}
	}
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

func (c *Client) replayPendingHookResults(ledger *rollbackLedger) {
	results, err := ledger.pendingResults()
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC Hook] cannot load pending hook results: %v", err)
		return
	}
	for _, result := range results {
		if err := monitorStream.sendCommandResult(result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Hook] failed to replay hook result request_id=%s: %v", result.GetRequestId(), err)
			return
		}
		if err := ledger.markDelivered(result.GetRequestId()); err != nil {
			logs.ErrorFile("grpc", "[gRPC Hook] failed to mark hook result delivered request_id=%s: %v", result.GetRequestId(), err)
			return
		}
	}
}
