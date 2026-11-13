package grpc

import (
	"sync"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
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

// HostSecurityProtocolVersion is the highest host_security option
// protocol this build understands.
const HostSecurityProtocolVersion = 1

// CapabilityHostSecurity is declared from the host security command
// implementation's initialization, like every other remote feature.
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

// SetHostSecurityHandler registers the remote host security implementation.
// The daemon wiring calls it before the client starts, so no command can
// arrive in the startup race window and be answered UNIMPLEMENTED.
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
// read-only: answered directly on bounded slots, never recorded in the
// ledger.
func isMutatingHostSecurityCommand(t string) bool {
	switch t {
	case HostSecurityCommandScanRun, HostSecurityCommandUpdatePreview,
		HostSecurityCommandUpdateApply, HostSecurityCommandUpdateVerify:
		return true
	}
	return false
}
