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

// Remote webhook management command dispatch. Webhook commands arrive over
// the MonitorStream like every remote command (MonitorCommandRequest with a
// webhook_* type and the WebhookOptions payload) and expose the SAME webhook
// subsystem the local CLI reads and writes (durable deployment jobs, the
// delivery ledger, and the app's phelix.yaml webhook section) — there is no
// second webhook implementation.
//
//   - queries (status/history/config/deliveries) are read-only and answered
//     synchronously without the ledger — safe to retry at will;
//   - mutations (enable/disable/set_branch/set_secret_env) are idempotent by
//     request_id through a durable ledger, exactly like remote rollbacks and
//     matrix commands: same request_id + same payload replays the stored
//     result, same request_id + different payload is ALREADY_EXISTS, and a
//     request in progress answers UNAVAILABLE;
//   - their results carry structured webhook state in additive
//     MonitorCommandResult fields, so the backend never parses human-readable
//     output;
//   - live deployment progress keeps flowing through the existing
//     webhook_job_* application events — the command channel carries only
//     command results.
//
// The cmd package registers the real implementation (SetWebhookHandler).

// Webhook command types (MonitorCommandRequest.type).
const (
	WebhookCommandStatus       = "webhook_status"
	WebhookCommandHistory      = "webhook_history"
	WebhookCommandConfig       = "webhook_config"
	WebhookCommandDeliveries   = "webhook_deliveries"
	WebhookCommandEnable       = "webhook_enable"
	WebhookCommandDisable      = "webhook_disable"
	WebhookCommandSetBranch    = "webhook_set_branch"
	WebhookCommandSetSecretEnv = "webhook_set_secret_env"
)

// webhookLedgerFile is the durable idempotency ledger for remote webhook
// mutations. Kept separate from the rollback and matrix ledgers so a corrupt
// or exhausted ledger cannot take the other features' idempotency down.
const webhookLedgerFile = "remote-webhook-ledger.json"

// webhookCommandSettleTimeout bounds how long daemon shutdown waits for
// in-flight webhook mutations (fast phelix.yaml edits; this covers
// finalization, ledger persistence and the result send).
const webhookCommandSettleTimeout = 5 * time.Second

// WebhookCommandSettleTimeout exposes the shutdown drain bound for the
// daemon wiring.
func WebhookCommandSettleTimeout() time.Duration {
	return webhookCommandSettleTimeout
}

// The webhook remote-management dispatch compiled into this build declares
// its capability: the backend may gate webhook_* commands on it.
func init() { RegisterCapability(CapabilityWebhookManagement) }

// WebhookHandlerFunc executes one webhook command and returns its fully
// populated result. The handler sets Status ("success"|"error"), Error and
// ErrorCode on failure, and the webhook_* payload fields appropriate for the
// command; the dispatcher stamps RequestId, Command, AppName and Timestamp.
type WebhookHandlerFunc func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult

var (
	webhookHandlerMu sync.RWMutex
	webhookHandler   WebhookHandlerFunc
)

// SetWebhookHandler registers the remote webhook implementation. The daemon
// wiring calls it before the client starts, so no command can arrive in the
// startup race window and be answered UNIMPLEMENTED.
func SetWebhookHandler(h WebhookHandlerFunc) {
	webhookHandlerMu.Lock()
	defer webhookHandlerMu.Unlock()
	webhookHandler = h
}

func getWebhookHandler() WebhookHandlerFunc {
	webhookHandlerMu.RLock()
	defer webhookHandlerMu.RUnlock()
	return webhookHandler
}

// webhookResults is the durable idempotency ledger for remote webhook
// mutations.
var webhookResults = newRollbackLedger(filepath.Join(server.DataDir(), webhookLedgerFile), rollbackLedgerCapacity, "webhook",
	"the configuration mutation may or may not have been applied — reconcile with a webhook_config command")

// InitializeWebhookLedger loads the durable remote-webhook idempotency
// ledger. Monitor startup must call it before accepting webhook commands; a
// corrupt or unreadable ledger fails closed (mutating webhook commands
// answer UNAVAILABLE instead of executing without idempotency). See
// InitializeRollbackLedger.
func InitializeWebhookLedger() error {
	return webhookResults.initializeAt(filepath.Join(server.DataDir(), webhookLedgerFile))
}

// WebhookLedgerPath exposes the effective webhook ledger location for
// startup diagnostics and integration tests.
func WebhookLedgerPath() string {
	webhookResults.mu.Lock()
	defer webhookResults.mu.Unlock()
	return webhookResults.path
}

// pendingWebhookWG tracks in-flight mutating webhook commands (execution +
// result persistence + send attempt) so daemon shutdown can drain them.
var pendingWebhookWG sync.WaitGroup

// WaitPendingWebhookCommands waits for in-flight webhook commands to settle,
// bounded by timeout.
func WaitPendingWebhookCommands(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		pendingWebhookWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logs.WarningFile("grpc", "[gRPC Webhook] shutdown drain timed out after %s", timeout)
	}
}

// webhookQueryLimitDefault and webhookQueryLimitMax bound list-shaped query
// answers (history, deliveries), matching the local CLI defaults.
const (
	webhookQueryLimitDefault = 20
	webhookQueryLimitMax     = 100
)

// WebhookQueryLimit resolves a request's limit option to the effective bound.
func WebhookQueryLimit(requested int32) int {
	if requested <= 0 {
		return webhookQueryLimitDefault
	}
	if requested > webhookQueryLimitMax {
		return webhookQueryLimitMax
	}
	return int(requested)
}

// isWebhookCommand reports whether a command type is a webhook command.
func isWebhookCommand(t string) bool {
	switch t {
	case WebhookCommandStatus, WebhookCommandHistory, WebhookCommandConfig,
		WebhookCommandDeliveries, WebhookCommandEnable, WebhookCommandDisable,
		WebhookCommandSetBranch, WebhookCommandSetSecretEnv:
		return true
	}
	return false
}

// isMutatingWebhookCommand reports whether the command changes state (and is
// therefore ledger-guarded). The four queries are read-only: they are
// answered directly, never recorded in the ledger, and never modify durable
// webhook state, job records or deduplication.
func isMutatingWebhookCommand(t string) bool {
	switch t {
	case WebhookCommandEnable, WebhookCommandDisable, WebhookCommandSetBranch, WebhookCommandSetSecretEnv:
		return true
	}
	return false
}

// validateWebhookCommand checks the transport-level shape of a webhook
// command before anything executes: correlation, foreign option fields
// (webhook commands must not carry rollback/rebuild/matrix options), and
// per-type option rules. Semantic validation (branch format, env-var name,
// resulting configuration validity) happens in the handler through the same
// project-package validation the local webhook server uses — there is no
// second validation rule set.
func validateWebhookCommand(req *pb.MonitorCommandRequest) error {
	if req.GetRequestId() == "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires a request_id", req.GetType())
	}
	if req.GetStrategy() != "" || req.GetReplicas() != 0 || req.GetReason() != "" ||
		req.GetVerifyDurationMs() != 0 || req.GetDryRun() || req.GetTarget() != "" || req.GetMatrix() != nil {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s command must not carry rollback/rebuild/matrix options (strategy, replicas, reason, verify_duration_ms, dry_run, target, matrix)", req.GetType())
	}
	if req.GetAppName() == "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires an app_name (the app's stable ID or its name)", req.GetType())
	}
	if !isWebhookCommand(req.GetType()) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unknown webhook command type %q", req.GetType())
	}

	opts := req.GetWebhook()
	if opts == nil {
		// Option-less requests are fine for every command except the two
		// setters, which are meaningless without their value.
		if req.GetType() == WebhookCommandSetBranch {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "webhook_set_branch requires webhook.branch")
		}
		if req.GetType() == WebhookCommandSetSecretEnv {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "webhook_set_secret_env requires webhook.secret_env")
		}
		return nil
	}

	switch req.GetType() {
	case WebhookCommandStatus, WebhookCommandConfig:
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command must not carry webhook options", req.GetType())
	case WebhookCommandHistory, WebhookCommandDeliveries:
		if opts.GetBranch() != "" || opts.GetSecretEnv() != "" || opts.GetEnabled() {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command accepts only the webhook.limit option", req.GetType())
		}
		if opts.GetLimit() < 0 {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument, "webhook limit must be 0 (default %d) or positive, got %d", webhookQueryLimitDefault, opts.GetLimit())
		}
		if opts.GetLimit() > webhookQueryLimitMax {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument, "webhook limit must not exceed %d, got %d", webhookQueryLimitMax, opts.GetLimit())
		}
	case WebhookCommandEnable:
		if opts.GetBranch() != "" || opts.GetSecretEnv() != "" || opts.GetLimit() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "webhook_enable accepts only the webhook.enabled option")
		}
		if opts.GetEnabled() {
			return nil // optional confirmation; must agree with the command
		}
		return phelixerr.New(phelixerr.CodeInvalidArgument, "webhook_enable carries enabled=false — use webhook_disable")
	case WebhookCommandDisable:
		if opts.GetBranch() != "" || opts.GetSecretEnv() != "" || opts.GetLimit() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "webhook_disable accepts only the webhook.enabled option")
		}
		if opts.GetEnabled() {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "webhook_disable carries enabled=true — use webhook_enable")
		}
	case WebhookCommandSetBranch:
		if opts.GetSecretEnv() != "" || opts.GetEnabled() || opts.GetLimit() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "webhook_set_branch accepts only the webhook.branch option")
		}
		if opts.GetBranch() == "" {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "webhook_set_branch requires webhook.branch")
		}
		if len(opts.GetBranch()) > 200 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "webhook branch is too long (max 200 characters)")
		}
	case WebhookCommandSetSecretEnv:
		if opts.GetBranch() != "" || opts.GetEnabled() || opts.GetLimit() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "webhook_set_secret_env accepts only the webhook.secret_env option")
		}
		if opts.GetSecretEnv() == "" {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "webhook_set_secret_env requires webhook.secret_env")
		}
		if len(opts.GetSecretEnv()) > 128 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "webhook secret_env is too long (max 128 characters)")
		}
	}
	return nil
}

// handleWebhookCommand dispatches one webhook command. Mutations are
// validated and ledger-begun synchronously, then executed on a background
// goroutine; their terminal result is persisted in the ledger BEFORE the
// send attempt and replayed on the next stream connection when the send
// could not be confirmed. Queries are answered synchronously without the
// ledger. Metrics are not paused — webhook commands are fast.
func (c *Client) handleWebhookCommand(req *pb.MonitorCommandRequest, ledger *rollbackLedger) {
	if err := validateWebhookCommand(req); err != nil {
		_ = sendCommandResultLogged(finalizeWebhookResult(req, commandErrorResult(req, err)))
		return
	}

	if !isMutatingWebhookCommand(req.GetType()) {
		_ = sendCommandResultLogged(executeWebhookCommand(req))
		return
	}

	outcome, replay, err := ledger.begin(req)
	if err != nil {
		_ = sendCommandResultLogged(finalizeWebhookResult(req, commandErrorResult(req, err)))
		return
	}
	if outcome == rollbackBeginReplay {
		if sendCommandResultLogged(replay) == nil {
			if err := ledger.markDelivered(req.GetRequestId()); err != nil {
				logs.ErrorFile("grpc", "[gRPC Webhook] failed to mark replay delivered: %v", err)
			}
		}
		return
	}

	pendingWebhookWG.Add(1)
	go func() {
		defer pendingWebhookWG.Done()
		result := executeWebhookCommand(req)
		if err := ledger.complete(req.GetRequestId(), result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Webhook] result persistence failed: %v", err)
			result = finalizeWebhookResult(req, commandErrorResult(req, phelixerr.Wrap(phelixerr.CodeUnavailable,
				"webhook command completed but its durable result could not be recorded", err)))
		} else if monitorStream.sendCommandResult(result) == nil {
			if err := ledger.markDelivered(req.GetRequestId()); err != nil {
				logs.ErrorFile("grpc", "[gRPC Webhook] failed to mark result delivered: %v", err)
			}
		}
		// A failed send leaves the entry pending: the next MonitorStream
		// connection replays it (replayPendingWebhookResults).
	}()
}

// executeWebhookCommand runs the registered handler with panic isolation —
// the daemon must survive a handler panic. The result always carries the
// command's identity fields and a structured error code.
func executeWebhookCommand(req *pb.MonitorCommandRequest) (result *pb.MonitorCommandResult) {
	handler := getWebhookHandler()
	if handler == nil {
		return finalizeWebhookResult(req, commandErrorResult(req,
			phelixerr.New(phelixerr.CodeUnimplemented, "webhook command not available: no webhook handler registered")))
	}
	defer func() {
		if rec := recover(); rec != nil {
			logs.ErrorFile("grpc", "[gRPC Webhook] handler panicked: %v", rec)
			result = finalizeWebhookResult(req, commandErrorResult(req,
				phelixerr.Newf(phelixerr.CodeUnknown, "webhook command panicked: %v", rec)))
		}
	}()
	result = handler(req)
	if result == nil {
		result = commandErrorResult(req, phelixerr.New(phelixerr.CodeUnknown, "webhook handler returned no result"))
	}
	return finalizeWebhookResult(req, result)
}

// finalizeWebhookResult stamps the transport-level identity fields the
// handler does not own, defaults the error code on error results without
// one, and redacts the error text.
func finalizeWebhookResult(req *pb.MonitorCommandRequest, result *pb.MonitorCommandResult) *pb.MonitorCommandResult {
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

// replayPendingWebhookResults re-sends webhook command results that were
// durably recorded but never confirmed delivered. Called on every
// MonitorStream (re)connection.
func (c *Client) replayPendingWebhookResults(ledger *rollbackLedger) {
	results, err := ledger.pendingResults()
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC Webhook] cannot load pending webhook results: %v", err)
		return
	}
	for _, result := range results {
		if err := monitorStream.sendCommandResult(result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Webhook] failed to replay webhook result request_id=%s: %v", result.GetRequestId(), err)
			return
		}
		if err := ledger.markDelivered(result.GetRequestId()); err != nil {
			logs.ErrorFile("grpc", "[gRPC Webhook] failed to mark webhook result delivered request_id=%s: %v", result.GetRequestId(), err)
			return
		}
	}
}
