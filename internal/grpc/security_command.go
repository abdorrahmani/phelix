package grpc

import (
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/protobuf/proto"
)

// Security command types (MonitorCommandRequest.type).
const (
	SecurityCommandReport    = "security_report"
	SecurityCommandHistory   = "security_history"
	SecurityCommandDetail    = "security_scan_detail"
	SecurityCommandAdvisory  = "security_advisory"
	SecurityCommandConfig    = "security_config"
	SecurityCommandRun       = "security_scan_run"
	SecurityCommandConfigSet = "security_config_set"
)

// CapabilitySecurityScan declares that this build can execute security
// commands; the backend may gate the security_* surface on it.
const CapabilitySecurityScan = "security_scan"

// securityLedgerFile is the durable idempotency ledger for remote security
// mutations, separate from the other features' ledgers.
const securityLedgerFile = "remote-security-ledger.json"

// securityCommandSettleTimeout bounds how long daemon shutdown waits for
// in-flight security mutations. A remote scan that outlives the drain
// keeps running as an independent execution and its record completes on
// disk; the result replays from the ledger on the next connection.
const securityCommandSettleTimeout = 10 * time.Second

// SecurityCommandSettleTimeout exposes the shutdown drain bound.
func SecurityCommandSettleTimeout() time.Duration {
	return securityCommandSettleTimeout
}

func init() { RegisterCapability(CapabilitySecurityScan) }

// SecurityHandlerFunc executes one security command and returns its fully
// populated result. The handler sets Status ("success"|"error"), Error and
// ErrorCode on failure, and the security_* payload field appropriate for
// the command; the dispatcher stamps RequestId, Command, AppName and
// Timestamp. The handler enforces reporting consent for the target app.
type SecurityHandlerFunc func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult

// SecurityAuthorizerFunc decides whether the remote principal may execute
// a security mutation against one application. Nil means no policy is
// configured and the local default applies.
type SecurityAuthorizerFunc func(req *pb.MonitorCommandRequest) error

var (
	securityHandlerMu sync.RWMutex
	securityHandler   SecurityHandlerFunc

	securityAuthorizerMu sync.RWMutex
	securityAuthorizer   SecurityAuthorizerFunc
)

// SetSecurityHandler registers the remote security implementation. The
// daemon wiring calls it before the client starts, so no command can
// arrive in the startup race window and be answered UNIMPLEMENTED.
func SetSecurityHandler(h SecurityHandlerFunc) {
	securityHandlerMu.Lock()
	defer securityHandlerMu.Unlock()
	securityHandler = h
}

func getSecurityHandler() SecurityHandlerFunc {
	securityHandlerMu.RLock()
	defer securityHandlerMu.RUnlock()
	return securityHandler
}

// SetSecurityAuthorizer registers the mutation authorization gate.
func SetSecurityAuthorizer(a SecurityAuthorizerFunc) {
	securityAuthorizerMu.Lock()
	defer securityAuthorizerMu.Unlock()
	securityAuthorizer = a
}

func getSecurityAuthorizer() SecurityAuthorizerFunc {
	securityAuthorizerMu.RLock()
	defer securityAuthorizerMu.RUnlock()
	return securityAuthorizer
}

// securityResults is the durable idempotency ledger for remote security
// mutations.
var securityResults = newRollbackLedger(filepath.Join(server.DataDir(), securityLedgerFile), 0, "security",
	"the security mutation may or may not have been applied; reconcile with a security_config or security_scan_detail command")

// InitializeSecurityLedger loads the durable remote-security idempotency
// ledger. Monitor startup must call it before accepting security
// commands; a corrupt or unreadable ledger fails closed.
func InitializeSecurityLedger() error {
	return securityResults.initializeAt(filepath.Join(server.DataDir(), securityLedgerFile))
}

// SecurityLedgerPath exposes the effective ledger location for diagnostics.
func SecurityLedgerPath() string {
	securityResults.mu.Lock()
	defer securityResults.mu.Unlock()
	return securityResults.path
}

// pendingSecurityWG tracks in-flight mutating security commands.
var pendingSecurityWG sync.WaitGroup
var securityQuerySlots = make(chan struct{}, 8)

// WaitPendingSecurityCommands waits for in-flight security commands to
// settle, bounded by timeout.
func WaitPendingSecurityCommands(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		pendingSecurityWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logs.WarningFile("grpc", "[gRPC Security] shutdown drain timed out after %s", timeout)
	}
}

// securityQueryLimitDefault and securityQueryLimitMax bound the history
// page size, matching the local CLI defaults.
const (
	securityQueryLimitDefault = 20
	securityQueryLimitMax     = 100
)

// SecurityQueryLimit resolves a request's limit option to the effective bound.
func SecurityQueryLimit(requested int32) int {
	if requested <= 0 {
		return securityQueryLimitDefault
	}
	if requested > securityQueryLimitMax {
		return securityQueryLimitMax
	}
	return int(requested)
}

// isSecurityCommand reports whether a command type is a security command.
func isSecurityCommand(t string) bool {
	switch t {
	case SecurityCommandReport, SecurityCommandHistory, SecurityCommandDetail,
		SecurityCommandAdvisory, SecurityCommandConfig, SecurityCommandRun,
		SecurityCommandConfigSet:
		return true
	}
	return false
}

// isMutatingSecurityCommand reports whether the command changes state (and
// is therefore ledger-guarded and authorization-gated). Queries are
// read-only: answered directly, never recorded in the ledger.
func isMutatingSecurityCommand(t string) bool {
	switch t {
	case SecurityCommandRun, SecurityCommandConfigSet:
		return true
	}
	return false
}

// securityFindingFilterActive reports whether any finding filter is set.
func securityFindingFilterActive(opts *pb.SecurityCommandOptions) bool {
	return opts.GetSeverity() != "" || opts.GetEcosystem() != "" || opts.GetState() != "" || opts.GetPackage() != ""
}

// validateSecurityCommand checks the transport-level shape of a security
// command before anything executes: correlation, foreign option fields,
// and per-type option rules. Semantic validation (interval format,
// severity vocabulary, resulting configuration validity) happens in the
// handler through the same project-package validation the local CLI uses.
func validateSecurityCommand(req *pb.MonitorCommandRequest) error {
	if req.GetRequestId() == "" || len(req.GetRequestId()) > 64 || strings.ContainsFunc(req.GetRequestId(), unicode.IsControl) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires a request_id", req.GetType())
	}
	if req.GetStrategy() != "" || req.GetReplicas() != 0 || req.GetReason() != "" ||
		req.GetVerifyDurationMs() != 0 || req.GetDryRun() || req.GetTarget() != "" || req.GetMatrix() != nil ||
		req.GetWebhook() != nil || req.GetLimit() != 0 || req.GetReplica() != "" || req.GetCanaryPercent() != 0 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s command must not carry rollback/rebuild/matrix/webhook options", req.GetType())
	}
	if req.GetAppName() == "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires an app_name (the app's stable ID or its name)", req.GetType())
	}
	if !isSecurityCommand(req.GetType()) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unknown security command type %q", req.GetType())
	}

	opts := req.GetSecurity()
	if opts == nil {
		switch req.GetType() {
		case SecurityCommandDetail:
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security_scan_detail requires security.scan_id")
		case SecurityCommandAdvisory:
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security_advisory requires security.advisory_id")
		case SecurityCommandConfigSet:
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security_config_set requires configuration fields")
		}
		return nil
	}
	if opts.GetProtocolVersion() > SecurityProtocolVersion {
		return phelixerr.New(phelixerr.CodeUnimplemented, "unsupported security protocol version")
	}
	if opts.GetEnabled() && !opts.GetSetEnabled() {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "enabled requires set_enabled")
	}
	if opts.GetFindingsOffset() < 0 || (opts.GetFindingsOffset() != 0 && req.GetType() != SecurityCommandReport && req.GetType() != SecurityCommandDetail) {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "findings_offset is non-negative and only valid for report or detail")
	}
	for _, filter := range [][2]string{{opts.GetSeverity(), "low medium high critical unknown"}, {opts.GetEcosystem(), "go rust"}, {opts.GetState(), "affected potentially_affected unknown not_affected"}} {
		if filter[0] != "" && !slices.Contains(strings.Fields(filter[1]), strings.ToLower(filter[0])) {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "invalid security finding filter")
		}
	}

	configFields := opts.GetSetEnabled() || opts.GetEnabled() || opts.GetScanInterval() != "" || opts.GetFailThreshold() != ""
	filterFields := securityFindingFilterActive(opts) || opts.GetIncludeGraph()

	switch req.GetType() {
	case SecurityCommandDetail:
		if configFields || opts.GetAdvisoryId() != "" || opts.GetLimit() != 0 || opts.GetOffset() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security_scan_detail must not carry configuration fields")
		}
		if opts.GetScanId() == "" {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security_scan_detail requires security.scan_id")
		}
	case SecurityCommandAdvisory:
		if configFields || filterFields || opts.GetScanId() != "" || opts.GetLimit() != 0 || opts.GetOffset() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security_advisory accepts only security.advisory_id")
		}
		if opts.GetAdvisoryId() == "" {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security_advisory requires security.advisory_id")
		}
	case SecurityCommandHistory:
		if configFields || opts.GetIncludeGraph() {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security_history must not carry configuration fields")
		}
		if opts.GetScanId() != "" || opts.GetAdvisoryId() != "" {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security_history accepts only paging and finding filters")
		}
		if opts.GetLimit() < 0 || opts.GetLimit() > securityQueryLimitMax {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"security_history limit must be 0 (default %d) or at most %d, got %d", securityQueryLimitDefault, securityQueryLimitMax, opts.GetLimit())
		}
		if opts.GetOffset() < 0 {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument, "security_history offset must not be negative, got %d", opts.GetOffset())
		}
	case SecurityCommandReport:
		if configFields || opts.GetScanId() != "" || opts.GetAdvisoryId() != "" ||
			opts.GetLimit() != 0 || opts.GetOffset() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security_report accepts only finding filters and include_graph")
		}
	case SecurityCommandRun:
		if configFields || filterFields || opts.GetScanId() != "" || opts.GetAdvisoryId() != "" ||
			opts.GetLimit() != 0 || opts.GetOffset() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security_scan_run carries no options; app_name identifies the target")
		}
	case SecurityCommandConfig:
		if configFields || filterFields || opts.GetScanId() != "" || opts.GetAdvisoryId() != "" ||
			opts.GetLimit() != 0 || opts.GetOffset() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security_config carries no options")
		}
	case SecurityCommandConfigSet:
		if filterFields || opts.GetScanId() != "" || opts.GetAdvisoryId() != "" ||
			opts.GetLimit() != 0 || opts.GetOffset() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security_config_set accepts only the set_* fields")
		}
		if !configFields {
			return phelixerr.New(phelixerr.CodeInvalidArgument,
				"security_config_set requires at least one of set_enabled, set_scan_interval, set_fail_threshold")
		}
		if err := (&project.SecurityConfig{Vulnerability: &project.VulnerabilityConfig{ScanInterval: project.ScanInterval(opts.GetScanInterval()), FailThreshold: project.ScanSeverity(opts.GetFailThreshold())}}).Validate(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeInvalidArgument, "invalid security settings", err)
		}
	}
	return nil
}

// handleSecurityCommand dispatches one security command. Mutations are
// validated and ledger-begun synchronously, then executed on a background
// goroutine; their terminal result is persisted in the ledger BEFORE the
// send attempt and replayed on the next stream connection when the send
// could not be confirmed. Queries use bounded background workers without
// the ledger.
func (c *Client) handleSecurityCommand(req *pb.MonitorCommandRequest, ledger *rollbackLedger) {
	if err := validateSecurityCommand(req); err != nil {
		_ = sendCommandResultLogged(finalizeSecurityResult(req, commandErrorResult(req, err)))
		return
	}

	if !isMutatingSecurityCommand(req.GetType()) {
		select {
		case securityQuerySlots <- struct{}{}:
			pendingSecurityWG.Add(1)
			go func() {
				defer pendingSecurityWG.Done()
				defer func() { <-securityQuerySlots }()
				_ = sendSecurityResult(executeSecurityCommand(req))
			}()
		default:
			_ = sendCommandResultLogged(commandErrorResult(req, phelixerr.New(phelixerr.CodeUnavailable, "security query capacity reached")))
		}
		return
	}

	outcome, replay, err := ledger.begin(req)
	if err != nil {
		_ = sendCommandResultLogged(finalizeSecurityResult(req, commandErrorResult(req, err)))
		return
	}
	if outcome == rollbackBeginReplay {
		_ = sendSecurityResult(replay)
		return
	}

	pendingSecurityWG.Add(1)
	go func() {
		defer pendingSecurityWG.Done()
		var result *pb.MonitorCommandResult
		if authorizer := getSecurityAuthorizer(); authorizer != nil {
			if err := authorizer(req); err != nil {
				result = finalizeSecurityResult(req, commandErrorResult(req, err))
			}
		}
		if result == nil {
			result = executeSecurityCommand(req)
		}
		if err := ledger.complete(req.GetRequestId(), result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Security] result persistence failed: %v", err)
			result = finalizeSecurityResult(req, commandErrorResult(req, phelixerr.Wrap(phelixerr.CodeSecurityOutcomeUnknown,
				"security command completed but its durable result could not be recorded", err)))
		}
		_ = sendSecurityResult(result)
	}()
}

// executeSecurityCommand runs the registered handler with panic isolation.
func executeSecurityCommand(req *pb.MonitorCommandRequest) (result *pb.MonitorCommandResult) {
	handler := getSecurityHandler()
	if handler == nil {
		return finalizeSecurityResult(req, commandErrorResult(req,
			phelixerr.New(phelixerr.CodeUnimplemented, "security command not available: no security handler registered")))
	}
	defer func() {
		if rec := recover(); rec != nil {
			logs.ErrorFile("grpc", "[gRPC Security] handler panicked: %v", rec)
			result = finalizeSecurityResult(req, commandErrorResult(req,
				phelixerr.Newf(phelixerr.CodeUnknown, "security command panicked: %v", rec)))
		}
	}()
	result = handler(req)
	if result == nil {
		result = commandErrorResult(req, phelixerr.New(phelixerr.CodeUnknown, "security handler returned no result"))
	}
	return finalizeSecurityResult(req, result)
}

// finalizeSecurityResult stamps the transport-level identity fields the
// handler does not own, defaults the error code on error results without
// one, and redacts the error text.
func finalizeSecurityResult(req *pb.MonitorCommandRequest, result *pb.MonitorCommandResult) *pb.MonitorCommandResult {
	result.RequestId = req.GetRequestId()
	result.Command = req.GetType()
	result.AppName = req.GetAppName()
	result.Timestamp = time.Now().UnixMilli()
	if proto.Size(result) > 3<<20 {
		return commandErrorResult(req, phelixerr.New(phelixerr.CodeSecurityResponseTooLarge, "security response exceeds 3 MiB; request a smaller page or omit the graph"))
	}
	if result.Status == "error" {
		if result.ErrorCode == "" {
			result.ErrorCode = string(phelixerr.CodeUnknown)
		}
		result.Error = phelixerr.Redact(result.Error)
	}
	return result
}

func sendSecurityResult(result *pb.MonitorCommandResult) error {
	if result.GetSecurityScanRecord() != nil || result.GetSecurityScanList() != nil || result.GetSecurityAdvisory() != nil || result.GetSecurityConfig() != nil {
		status := executeSecurityCommand(&pb.MonitorCommandRequest{RequestId: result.GetRequestId(), Type: SecurityCommandConfig, AppName: result.GetAppName()})
		if result.GetCommand() == SecurityCommandConfig {
			return sendCommandResultLogged(status)
		}
		if status.GetStatus() != "success" || status.GetSecurityConfig().GetReporting() != "enabled" {
			code := status.GetErrorCode()
			if code == "" {
				code = string(phelixerr.CodeSecurityReportingDisabled)
			}
			result = proto.Clone(result).(*pb.MonitorCommandResult)
			result.SecurityScanRecord = nil
			result.SecurityScanList = nil
			result.SecurityAdvisory = nil
			result.SecurityConfig = nil
			result.SecurityResultWithheld = true
			result.SecurityConsentErrorCode = code
			if !isMutatingSecurityCommand(result.GetCommand()) {
				result.Status = "error"
				result.ErrorCode = code
				result.Error = "security data is withheld by current local configuration"
			}
		}
	}
	return sendCommandResultLogged(result)
}

func acknowledgeSecurityResult(ledger *rollbackLedger, ack *pb.SecurityResultAck) {
	ledger.mu.Lock()
	entry := ledger.entries[ack.GetRequestId()]
	valid := entry != nil && entry.State == ledgerStateComplete && entry.Result.GetCommand() == ack.GetCommand() && entry.Result.GetAppName() == ack.GetAppName()
	ledger.mu.Unlock()
	if valid {
		if err := ledger.markDelivered(ack.GetRequestId()); err != nil {
			logs.ErrorFile("grpc", "security result acknowledgement failed: %v", err)
		}
	}
}

// replayPendingSecurityResults re-sends security mutation results that
// were durably recorded but never confirmed delivered. Called on every
// MonitorStream (re)connection.
func (c *Client) replayPendingSecurityResults(ledger *rollbackLedger) {
	results, err := ledger.pendingResults()
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC Security] cannot load pending security results: %v", err)
		return
	}
	for _, result := range results {
		if err := sendSecurityResult(result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Security] failed to replay security result request_id=%s: %v", result.GetRequestId(), err)
			return
		}
	}
}

// SecuritySnapshotProvider returns the latest completed scan snapshot per
// consenting application. The daemon wiring supplies it; nil disables the
// snapshot push.
var SecuritySnapshotProvider func() []*pb.SecurityScanSnapshot

// sendSecuritySnapshots pushes the latest completed scan summary for every
// application with reporting consent. Idempotent by construction: the
// backend upserts by scan id.
func (c *Client) sendSecuritySnapshots() {
	provider := SecuritySnapshotProvider
	if provider == nil {
		return
	}
	for _, snapshot := range provider() {
		event := &pb.MonitorEvent{
			ServerId:  server.GetAgentID(),
			Timestamp: time.Now().UnixMilli(),
			Payload:   &pb.MonitorEvent_SecurityScan{SecurityScan: snapshot},
		}
		if err := monitorStream.send(event); err != nil {
			logs.ErrorFile("grpc", "[gRPC Security] failed to send scan snapshot: %v", err)
			return
		}
	}
}
