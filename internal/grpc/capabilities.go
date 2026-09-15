package grpc

import (
	"sort"
	"sync"
)

// Agent capabilities: the feature-gating vocabulary the backend consumes
// from CLIMetadata.capabilities. The agent declares what its build actually
// implements; the backend gates feature-specific commands on the specific
// entry (e.g. matrix_* on "build_matrix") instead of guessing from the
// version string — source builds carry no reliable version.
//
// Semantics: a capability describes the AGENT BUILD, not the process that
// happens to sync metadata. Two processes of the same binary can sync
// metadata for the same agent identity (the monitor daemon and the detached
// health-daemon child both start a gRPC client); they must report the same
// capability set, so the last-writer-wins backend record never flaps. For
// that reason capabilities are declared by the feature's implementation
// module at init (below), not derived from runtime handler registration.
//
// Backward compatibility: older agents predate the field entirely and send
// nothing — the backend must treat an absent/empty list as "baseline agent"
// and gate feature commands on the specific entry, never on the field's
// presence.
const (
	// CapabilityMonitoring: the agent runs the MonitorStream — server/app
	// metrics, logs, ServerInfo, and the remote lifecycle command channel
	// (start/stop/restart/remove/rebuild).
	CapabilityMonitoring = "monitoring"
	// CapabilityDeployment: the agent implements zero-downtime deployment
	// (blue-green/rolling/canary/progressive) — remote rebuild with strategy
	// overrides and DeploymentEvent/DeploymentSnapshot telemetry.
	CapabilityDeployment = "deployment"
	// CapabilityRollback: the agent implements remote rollback through the
	// existing rollback engine (the "rollback" MonitorStream command and its
	// durable idempotency ledger).
	CapabilityRollback = "rollback"
	// CapabilityBuildMatrix: the agent implements the remote Build Matrix
	// (matrix_* MonitorStream commands driving the existing matrix engine,
	// ReportMatrixEvent lifecycle events, and run-state resync).
	CapabilityBuildMatrix = "build_matrix"
	// CapabilityWebhookManagement: the agent implements remote webhook
	// management (webhook_* MonitorStream commands reading and mutating the
	// existing webhook subsystem — durable jobs, delivery ledger, and the
	// phelix.yaml webhook configuration).
	CapabilityWebhookManagement = "webhook_management"
)

// agentCapabilities is the registry of capabilities this agent build
// supports. Each remote feature registers its entry next to its own
// implementation (see the RegisterCapability call sites in monitor_stream.go,
// deployment_reporter.go, rollback_command.go, matrix_command.go) — the
// registry is the single source of truth; nothing else re-lists capability
// names.
var (
	agentCapabilitiesMu sync.Mutex
	agentCapabilities   = make(map[string]struct{})
)

// RegisterCapability declares one capability supported by this agent build.
// Called from the implementing feature's package initialization; registering
// the same name twice is a no-op (capabilities can never duplicate).
func RegisterCapability(name string) {
	agentCapabilitiesMu.Lock()
	defer agentCapabilitiesMu.Unlock()
	agentCapabilities[name] = struct{}{}
}

// unregisterCapability removes a capability (tests only).
func unregisterCapability(name string) {
	agentCapabilitiesMu.Lock()
	defer agentCapabilitiesMu.Unlock()
	delete(agentCapabilities, name)
}

// AgentCapabilities returns the capabilities this agent build supports as a
// deterministically sorted, duplicate-free list. It never returns nil — a
// build without remote features reports an empty list (a non-nil empty
// slice), which the backend reads as "baseline agent". The list is computed
// fresh on every call so periodic metadata refreshes reflect the current
// registry, never a stale snapshot.
func AgentCapabilities() []string {
	agentCapabilitiesMu.Lock()
	defer agentCapabilitiesMu.Unlock()
	out := make([]string, 0, len(agentCapabilities))
	for name := range agentCapabilities {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
