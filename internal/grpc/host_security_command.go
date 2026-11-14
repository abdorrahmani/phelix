package grpc

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/hostsecurity"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/protobuf/proto"
)

// Host security command types (MonitorCommandRequest.type).
const (
	HostSecurityCommandReport        = "host_security_report"
	HostSecurityCommandHistory       = "host_security_history"
	HostSecurityCommandDetail        = "host_security_scan_detail"
	HostSecurityCommandInventory     = "host_security_inventory"
	HostSecurityCommandConfig        = "host_security_config"
	HostSecurityCommandRebootStatus  = "host_security_reboot_status"
	HostSecurityCommandScanRun       = "host_security_scan_run"
	HostSecurityCommandUpdatePreview = "host_security_update_preview"
	HostSecurityCommandUpdateApply   = "host_security_update_apply"
	HostSecurityCommandUpdateVerify  = "host_security_update_verify"
)

// HostSecurityProtocolVersion is the highest host_security option protocol
// this build understands.
const HostSecurityProtocolVersion = 1

// hostSecurityLedgerFile is the durable idempotency ledger for remote host
// security mutations, separate from the other families' ledgers.
const hostSecurityLedgerFile = "remote-host-security-ledger.json"

// hostSecurityCommandSettleTimeout bounds how long daemon shutdown waits
// for in-flight host security mutations. A remote operation that outlives
// the drain keeps running independently; its result replays from the ledger
// on the next connection.
const hostSecurityCommandSettleTimeout = 10 * time.Second

// HostSecurityCommandSettleTimeout exposes the shutdown drain bound.
func HostSecurityCommandSettleTimeout() time.Duration {
	return hostSecurityCommandSettleTimeout
}

// hostSecurityQueryLimitDefault and hostSecurityQueryLimitMax bound the
// history and inventory page size.
const (
	hostSecurityQueryLimitDefault = 20
	hostSecurityQueryLimitMax     = 100
)

// hostSecurityMaxPackages bounds the per-command package target list.
const hostSecurityMaxPackages = 50

func init() { RegisterCapability(CapabilityHostSecurity) }

// HostSecurityHandlerFunc executes one host security command and returns
// its fully populated result. The handler sets Status ("success"|"error"),
// Error and ErrorCode on failure, and the host_security_* payload field
// appropriate for the command; the dispatcher stamps RequestId, Command and
// Timestamp. The handler enforces local host reporting consent.
type HostSecurityHandlerFunc func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult

// HostSecurityAuthorizerFunc decides whether the remote principal may
// execute a host security mutation on this host. Nil means no policy is
// configured and the local default applies.
type HostSecurityAuthorizerFunc func(req *pb.MonitorCommandRequest) error

var (
	hostSecurityHandlerMu sync.RWMutex
	hostSecurityHandler   HostSecurityHandlerFunc

	hostSecurityAuthorizerMu sync.RWMutex
	hostSecurityAuthorizer   HostSecurityAuthorizerFunc
)

// SetHostSecurityHandler registers the remote host security
// implementation. The daemon wiring calls it before the client starts, so
// no command can arrive in the startup race window and be answered
// UNIMPLEMENTED.
func SetHostSecurityHandler(h HostSecurityHandlerFunc) {
	hostSecurityHandlerMu.Lock()
	defer hostSecurityHandlerMu.Unlock()
	hostSecurityHandler = h
}

func getHostSecurityHandler() HostSecurityHandlerFunc {
	hostSecurityHandlerMu.RLock()
	defer hostSecurityHandlerMu.RUnlock()
	return hostSecurityHandler
}

// SetHostSecurityAuthorizer registers the mutation authorization gate.
func SetHostSecurityAuthorizer(a HostSecurityAuthorizerFunc) {
	hostSecurityAuthorizerMu.Lock()
	defer hostSecurityAuthorizerMu.Unlock()
	hostSecurityAuthorizer = a
}

func getHostSecurityAuthorizer() HostSecurityAuthorizerFunc {
	hostSecurityAuthorizerMu.RLock()
	defer hostSecurityAuthorizerMu.RUnlock()
	return hostSecurityAuthorizer
}

// hostSecurityResults is the durable idempotency ledger for remote host
// security mutations.
var hostSecurityResults = newRollbackLedger(filepath.Join(server.DataDir(), hostSecurityLedgerFile), rollbackLedgerCapacity, "host_security",
	"the host security operation may or may not have been applied; reconcile with a host_security_config or host_security_scan_detail command")

// InitializeHostSecurityLedger loads the durable remote host security
// idempotency ledger. Monitor startup must call it before accepting host
// security commands; a corrupt or unreadable ledger fails closed.
func InitializeHostSecurityLedger() error {
	return hostSecurityResults.initializeAt(filepath.Join(server.DataDir(), hostSecurityLedgerFile))
}

// HostSecurityLedgerPath exposes the effective ledger location for
// diagnostics.
func HostSecurityLedgerPath() string {
	hostSecurityResults.mu.Lock()
	defer hostSecurityResults.mu.Unlock()
	return hostSecurityResults.path
}

// pendingHostSecurityWG tracks in-flight mutating host security commands.
var pendingHostSecurityWG sync.WaitGroup
var hostSecurityQuerySlots = make(chan struct{}, 8)

// WaitPendingHostSecurityCommands waits for in-flight host security
// commands to settle, bounded by timeout.
func WaitPendingHostSecurityCommands(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		pendingHostSecurityWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logs.WarningFile("grpc", "[gRPC Host Security] shutdown drain timed out after %s", timeout)
	}
}

// isHostSecurityCommand reports whether a command type belongs to the host
// security family.
func isHostSecurityCommand(t string) bool {
	switch t {
	case HostSecurityCommandReport, HostSecurityCommandHistory, HostSecurityCommandDetail,
		HostSecurityCommandInventory, HostSecurityCommandConfig, HostSecurityCommandRebootStatus,
		HostSecurityCommandScanRun, HostSecurityCommandUpdatePreview,
		HostSecurityCommandUpdateApply, HostSecurityCommandUpdateVerify:
		return true
	}
	return false
}

// isMutatingHostSecurityCommand reports whether the command changes state
// (and is therefore ledger-guarded and authorization-gated). Queries are
// read-only: answered on bounded slots, never recorded in the ledger.
func isMutatingHostSecurityCommand(t string) bool {
	switch t {
	case HostSecurityCommandScanRun, HostSecurityCommandUpdatePreview,
		HostSecurityCommandUpdateApply, HostSecurityCommandUpdateVerify:
		return true
	}
	return false
}

// validateHostSecurityCommand checks the transport-level shape of a host
// security command before anything executes: correlation, foreign option
// fields, and per-type option rules. Semantic validation (package specs,
// operation state) happens in the handler through the same validation the
// local CLI uses.
func validateHostSecurityCommand(req *pb.MonitorCommandRequest) error {
	if req.GetRequestId() == "" || len(req.GetRequestId()) > 64 || strings.ContainsFunc(req.GetRequestId(), unicode.IsControl) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires a request_id", req.GetType())
	}
	if req.GetAppName() != "" || req.GetStrategy() != "" || req.GetReplicas() != 0 || req.GetReason() != "" ||
		req.GetVerifyDurationMs() != 0 || req.GetDryRun() || req.GetTarget() != "" || req.GetMatrix() != nil ||
		req.GetWebhook() != nil || req.GetLimit() != 0 || req.GetReplica() != "" || req.GetCanaryPercent() != 0 ||
		req.GetSecurity() != nil || req.GetJob() != nil || req.GetHooks() != nil {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s command must not carry app_name or any other command family's options", req.GetType())
	}
	if !isHostSecurityCommand(req.GetType()) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unknown host security command type %q", req.GetType())
	}

	opts := req.GetHostSecurity()
	if opts == nil {
		switch req.GetType() {
		case HostSecurityCommandDetail:
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_scan_detail requires host_security.scan_id")
		case HostSecurityCommandUpdatePreview:
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_update_preview requires host_security.packages")
		case HostSecurityCommandUpdateApply:
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_update_apply requires host_security.packages and host_security.preview_token")
		case HostSecurityCommandUpdateVerify:
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_update_verify requires host_security.operation_id")
		}
		return nil
	}
	if opts.GetProtocolVersion() > HostSecurityProtocolVersion {
		return phelixerr.New(phelixerr.CodeUnimplemented, "unsupported host security protocol version")
	}
	if opts.GetFindingsOffset() < 0 || (opts.GetFindingsOffset() != 0 && req.GetType() != HostSecurityCommandReport && req.GetType() != HostSecurityCommandDetail) {
		return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "findings_offset is non-negative and only valid for report or detail")
	}
	if opts.GetLimit() < 0 || opts.GetLimit() > hostSecurityQueryLimitMax {
		return phelixerr.Newf(phelixerr.CodeHostSecurityInvalid,
			"limit must be 0 (default %d) or at most %d, got %d", hostSecurityQueryLimitDefault, hostSecurityQueryLimitMax, opts.GetLimit())
	}
	if opts.GetOffset() < 0 {
		return phelixerr.Newf(phelixerr.CodeHostSecurityInvalid, "offset must not be negative, got %d", opts.GetOffset())
	}
	for _, filter := range [][2]string{{opts.GetSeverity(), "critical high medium low informational unknown"}, {opts.GetState(), "confirmed unverified potential"}} {
		if filter[0] != "" && !containsWord(filter[1], strings.ToLower(filter[0])) {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "invalid host security finding filter")
		}
	}
	for _, spec := range opts.GetPackages() {
		if err := hostsecurity.ValidatePackageSpec(spec); err != nil {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, err.Error())
		}
	}

	queryFields := opts.GetSeverity() != "" || opts.GetState() != "" || opts.GetPackage() != "" || opts.GetFindingsOffset() != 0
	pagingFields := opts.GetLimit() != 0 || opts.GetOffset() != 0
	offsetField := opts.GetOffset() != 0
	targetFields := len(opts.GetPackages()) > 0 || opts.GetPreviewToken() != "" || opts.GetOperationId() != ""

	switch req.GetType() {
	case HostSecurityCommandReport:
		if targetFields || opts.GetScanId() != "" || offsetField {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_report accepts only finding filters, findings_offset and limit")
		}
	case HostSecurityCommandHistory:
		if targetFields || opts.GetScanId() != "" || queryFields {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_history accepts only paging")
		}
	case HostSecurityCommandDetail:
		if targetFields || offsetField {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_scan_detail accepts only scan_id, finding filters, findings_offset and limit")
		}
		if opts.GetScanId() == "" {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_scan_detail requires host_security.scan_id")
		}
	case HostSecurityCommandInventory:
		if targetFields || opts.GetSeverity() != "" || opts.GetState() != "" || opts.GetFindingsOffset() != 0 {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_inventory accepts only scan_id, package filter and paging")
		}
	case HostSecurityCommandConfig, HostSecurityCommandRebootStatus, HostSecurityCommandScanRun:
		if targetFields || opts.GetScanId() != "" || queryFields || pagingFields {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "the command carries no host_security options")
		}
	case HostSecurityCommandUpdatePreview:
		if opts.GetPreviewToken() != "" || opts.GetOperationId() != "" || opts.GetScanId() != "" || queryFields || pagingFields {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_update_preview accepts only host_security.packages")
		}
		if len(opts.GetPackages()) == 0 {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_update_preview requires host_security.packages")
		}
		if len(opts.GetPackages()) > hostSecurityMaxPackages {
			return phelixerr.Newf(phelixerr.CodeHostSecurityInvalid, "at most %d packages per operation, got %d", hostSecurityMaxPackages, len(opts.GetPackages()))
		}
	case HostSecurityCommandUpdateApply:
		if opts.GetScanId() != "" || queryFields || pagingFields {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_update_apply accepts only packages and preview_token")
		}
		if len(opts.GetPackages()) == 0 {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_update_apply requires host_security.packages")
		}
		if len(opts.GetPackages()) > hostSecurityMaxPackages {
			return phelixerr.Newf(phelixerr.CodeHostSecurityInvalid, "at most %d packages per operation, got %d", hostSecurityMaxPackages, len(opts.GetPackages()))
		}
		if opts.GetPreviewToken() == "" {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_update_apply requires host_security.preview_token")
		}
	case HostSecurityCommandUpdateVerify:
		if len(opts.GetPackages()) > 0 || opts.GetPreviewToken() != "" || opts.GetScanId() != "" || queryFields || pagingFields {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_update_verify accepts only host_security.operation_id")
		}
		if opts.GetOperationId() == "" {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "host_security_update_verify requires host_security.operation_id")
		}
	}
	return nil
}

// containsWord reports whether the space-separated vocabulary contains the
// exact word.
func containsWord(vocabulary, word string) bool {
	for _, field := range strings.Fields(vocabulary) {
		if field == word {
			return true
		}
	}
	return false
}

// handleHostSecurityCommand dispatches one host security command. Mutations
// are validated and ledger-begun synchronously, then executed on a
// background goroutine; their terminal result is persisted in the ledger
// BEFORE the send attempt and replayed on the next stream connection when
// the send could not be confirmed by a SecurityResultAck. Queries use
// bounded background workers without the ledger.
func (c *Client) handleHostSecurityCommand(req *pb.MonitorCommandRequest, ledger *rollbackLedger) {
	if err := validateHostSecurityCommand(req); err != nil {
		_ = sendCommandResultLogged(finalizeHostSecurityResult(req, commandErrorResult(req, err)))
		return
	}

	if !isMutatingHostSecurityCommand(req.GetType()) {
		select {
		case hostSecurityQuerySlots <- struct{}{}:
			pendingHostSecurityWG.Add(1)
			go func() {
				defer pendingHostSecurityWG.Done()
				defer func() { <-hostSecurityQuerySlots }()
				_ = sendHostSecurityResult(executeHostSecurityCommand(req))
			}()
		default:
			_ = sendCommandResultLogged(commandErrorResult(req, phelixerr.New(phelixerr.CodeUnavailable, "host security query capacity reached")))
		}
		return
	}

	outcome, replay, err := ledger.begin(req)
	if err != nil {
		_ = sendCommandResultLogged(finalizeHostSecurityResult(req, commandErrorResult(req, err)))
		return
	}
	if outcome == rollbackBeginReplay {
		_ = sendHostSecurityResult(replay)
		return
	}

	pendingHostSecurityWG.Add(1)
	go func() {
		defer pendingHostSecurityWG.Done()
		var result *pb.MonitorCommandResult
		if authorizer := getHostSecurityAuthorizer(); authorizer != nil {
			if err := authorizer(req); err != nil {
				result = finalizeHostSecurityResult(req, commandErrorResult(req, err))
			}
		}
		if result == nil {
			result = executeHostSecurityCommand(req)
		}
		if err := ledger.complete(req.GetRequestId(), result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Host Security] result persistence failed: %v", err)
			result = finalizeHostSecurityResult(req, commandErrorResult(req, phelixerr.Wrap(phelixerr.CodeUnavailable,
				"host security command completed but its durable result could not be recorded", err)))
		}
		_ = sendHostSecurityResult(result)
	}()
}

// executeHostSecurityCommand runs the registered handler with panic
// isolation.
func executeHostSecurityCommand(req *pb.MonitorCommandRequest) (result *pb.MonitorCommandResult) {
	handler := getHostSecurityHandler()
	if handler == nil {
		return finalizeHostSecurityResult(req, commandErrorResult(req,
			phelixerr.New(phelixerr.CodeUnimplemented, "host security command not available: no host security handler registered")))
	}
	defer func() {
		if rec := recover(); rec != nil {
			logs.ErrorFile("grpc", "[gRPC Host Security] handler panicked: %v", rec)
			result = finalizeHostSecurityResult(req, commandErrorResult(req,
				phelixerr.Newf(phelixerr.CodeUnknown, "host security command panicked: %v", rec)))
		}
	}()
	result = handler(req)
	if result == nil {
		result = commandErrorResult(req, phelixerr.New(phelixerr.CodeUnknown, "host security handler returned no result"))
	}
	return finalizeHostSecurityResult(req, result)
}

// finalizeHostSecurityResult stamps the transport-level identity fields the
// handler does not own, defaults the error code on error results without
// one, and redacts the error text.
func finalizeHostSecurityResult(req *pb.MonitorCommandRequest, result *pb.MonitorCommandResult) *pb.MonitorCommandResult {
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

// sendHostSecurityResult delivers a host security result under the send-time
// consent gate. A query answer is delivered with its payload stripped and
// the withholding markers carry the consent error. A mutation result is
// never delivered in stripped form: withholding it stores nothing over the
// durable result, which stays pending in the ledger and replays in full once
// consent returns, so a payload-bearing success can never be replaced by a
// payload-stripped one. The config answer is never withheld: it is how the
// dashboard renders the disabled state.
func sendHostSecurityResult(result *pb.MonitorCommandResult) error {
	if !carriesHostSecurityPayload(result) || result.GetCommand() == HostSecurityCommandConfig ||
		hostsecurity.HostReportingEnabled(server.DataDir()) {
		return sendCommandResultLogged(result)
	}
	if isMutatingHostSecurityCommand(result.GetCommand()) {
		logs.WarningFile("grpc", "[gRPC Host Security] mutation result request_id=%s stays pending while consent is disabled; it replays when consent returns", result.GetRequestId())
		return nil
	}
	stripped := proto.Clone(result).(*pb.MonitorCommandResult)
	stripped.HostSecurityScanRecord = nil
	stripped.HostSecurityScanList = nil
	stripped.HostSecurityInventory = nil
	stripped.HostSecurityPreview = nil
	stripped.HostSecurityUpdate = nil
	stripped.HostSecurityReboot = nil
	stripped.SecurityResultWithheld = true
	stripped.SecurityConsentErrorCode = string(phelixerr.CodeHostSecurityConsentDisabled)
	stripped.Status = "error"
	stripped.ErrorCode = string(phelixerr.CodeHostSecurityConsentDisabled)
	stripped.Error = "host security data is withheld by current local configuration"
	return sendCommandResultLogged(stripped)
}

// carriesHostSecurityPayload reports whether the result carries host
// security data that consent governs.
func carriesHostSecurityPayload(result *pb.MonitorCommandResult) bool {
	return result.GetHostSecurityScanRecord() != nil || result.GetHostSecurityScanList() != nil ||
		result.GetHostSecurityInventory() != nil || result.GetHostSecurityPreview() != nil ||
		result.GetHostSecurityUpdate() != nil || result.GetHostSecurityReboot() != nil
}

// acknowledgeHostSecurityResult marks a durably-recorded host security
// mutation result as delivered when the backend acknowledges it.
func acknowledgeHostSecurityResult(ledger *rollbackLedger, ack *pb.SecurityResultAck) {
	ledger.mu.Lock()
	entry := ledger.entries[ack.GetRequestId()]
	valid := entry != nil && entry.State == ledgerStateComplete && entry.Result.GetCommand() == ack.GetCommand() && entry.Result.GetAppName() == ack.GetAppName()
	ledger.mu.Unlock()
	if valid {
		if err := ledger.markDelivered(ack.GetRequestId()); err != nil {
			logs.ErrorFile("grpc", "host security result acknowledgement failed: %v", err)
		}
	}
}

// replayPendingHostSecurityResults re-sends host security mutation results
// that were durably recorded but never confirmed delivered. Called on every
// MonitorStream (re)connection.
func (c *Client) replayPendingHostSecurityResults(ledger *rollbackLedger) {
	results, err := ledger.pendingResults()
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC Host Security] cannot load pending host security results: %v", err)
		return
	}
	for _, result := range results {
		if err := sendHostSecurityResult(result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Host Security] failed to replay host security result request_id=%s: %v", result.GetRequestId(), err)
			return
		}
	}
}

// HostSecuritySnapshotProvider returns the latest completed host scan
// summary when reporting consent is enabled, or nil otherwise. The daemon
// wiring supplies it; nil disables the snapshot push.
var HostSecuritySnapshotProvider func() *pb.HostSecuritySnapshotSummary

// sendHostSecuritySnapshots pushes the latest completed host scan summary.
// Idempotent by construction: the backend upserts by scan id.
func (c *Client) sendHostSecuritySnapshots() {
	provider := HostSecuritySnapshotProvider
	if provider == nil {
		return
	}
	snapshot := provider()
	if snapshot == nil {
		return
	}
	event := &pb.MonitorEvent{
		ServerId:  server.GetAgentID(),
		Timestamp: time.Now().UnixMilli(),
		Payload:   &pb.MonitorEvent_HostSecuritySummary{HostSecuritySummary: snapshot},
	}
	if err := monitorStream.send(event); err != nil {
		logs.ErrorFile("grpc", "[gRPC Host Security] failed to send host scan snapshot: %v", err)
	}
}
