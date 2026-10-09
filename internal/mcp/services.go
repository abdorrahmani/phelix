// Package mcp is Phelix's Model Context Protocol adapter: a thin, local,
// stdio-only transport that exposes a small set of EXISTING Phelix machine
// capabilities (context/inspect reads, plan create/show/list/apply, operation
// status) to autonomous coding agents. It is a protocol boundary, not a second
// execution engine — every tool delegates to the same validation, plan,
// authorization and execution pipeline the CLI uses, through the [Services]
// seam, and returns the same [machine.Envelope] the CLI's --json mode emits.
//
// Security posture: tool inputs are untrusted. The adapter never accepts a
// caller-supplied identity, role, approval, or authorization decision; the
// authorization boundary (internal/authz) and plan integrity checks
// (internal/plans) are preserved exactly. See docs/guides/mcp.md.
package mcp

import (
	"context"

	"github.com/abdorrahmani/phelix/internal/machine"
)

// Tool names. Stable, descriptive, and snake_cased with a phelix_ prefix so a
// client's tool list is unambiguous. These are part of the adapter's external
// contract — do not rename.
const (
	ToolContext         = "phelix_context"
	ToolInspect         = "phelix_inspect"
	ToolPlanShow        = "phelix_plan_show"
	ToolPlanList        = "phelix_plan_list"
	ToolOperationStatus = "phelix_operation_status"
	ToolPlanCreate      = "phelix_plan_create"
	ToolPlanApply       = "phelix_plan_apply"
)

// Bounds mirror the CLI's own context/list limits (cmd/context_model.go,
// internal/plans.List) so the MCP surface is bounded identically to --json CLI
// output. A zero request selects the default; anything above the max is capped
// by the service layer (the same validateContextLimit path the CLI uses).
const (
	DefaultVersions   = 20
	MaxVersions       = 100
	DefaultOperations = 20
	MaxOperations     = 100
	DefaultLogLines   = 100
	MaxLogLines       = 1000
	DefaultPlanList   = 20
	MaxPlanList       = 100
)

// Inspect resource types. This is the explicit, validated enum the
// phelix_inspect tool accepts; it mirrors the `phelix inspect <topic>`
// subcommands. The adapter rejects anything else as an invocation error before
// a service is called.
const (
	ResourceProject      = "project"
	ResourceRuntime      = "runtime"
	ResourceConfig       = "config"
	ResourceCapabilities = "capabilities"
	ResourceOperations   = "operations"
	ResourceApp          = "app"
	ResourceDeployment   = "deployment"
	ResourceVersions     = "versions"
	ResourceHealth       = "health"
)

var inspectResources = []string{
	ResourceProject, ResourceRuntime, ResourceConfig, ResourceCapabilities,
	ResourceOperations, ResourceApp, ResourceDeployment, ResourceVersions, ResourceHealth,
}

// InspectResources returns the valid phelix_inspect resource types in a stable
// order (used for the tool description and tests).
func InspectResources() []string {
	out := make([]string, len(inspectResources))
	copy(out, inspectResources)
	return out
}

// ValidInspectResource reports whether r is a known inspect resource type.
func ValidInspectResource(r string) bool {
	for _, k := range inspectResources {
		if k == r {
			return true
		}
	}
	return false
}

// Plan actions the create tool accepts — exactly the Phase 3 plan actions.
const (
	ActionRebuild  = "rebuild"
	ActionRollback = "rollback"
)

// --- tool input schemas (the SDK derives JSON Schema from these structs) ---
//
// Required fields have no `omitempty`; optional fields do. Every field is a
// typed scalar/enum/bounded int — never a raw filesystem path, shell command,
// executable name or environment value (see docs/guides/mcp.md, Security).

// ContextInput is the input to phelix_context.
type ContextInput struct {
	App        string `json:"app,omitempty" jsonschema:"optional application id or name; when set, app-scoped sections (deployment, versions, health, operations, logs) are included"`
	Versions   int    `json:"versions,omitempty" jsonschema:"max versions in the snapshot; 0 selects the default (20), capped at 100"`
	Operations int    `json:"operations,omitempty" jsonschema:"max operations in the snapshot; 0 selects the default (20), capped at 100"`
	LogLines   int    `json:"log_lines,omitempty" jsonschema:"max log lines in the snapshot; 0 selects the default (100), capped at 1000"`
}

// InspectInput is the input to phelix_inspect.
type InspectInput struct {
	Resource string `json:"resource" jsonschema:"resource to inspect: one of project, runtime, config, capabilities, operations, app, deployment, versions, health"`
	App      string `json:"app,omitempty" jsonschema:"application id or name; required for app, deployment, versions and health"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max items for the versions and operations resources; 0 selects the default (20), capped at 100"`
}

// PlanShowInput is the input to phelix_plan_show.
type PlanShowInput struct {
	PlanID string `json:"plan_id" jsonschema:"plan id to load: pln_ followed by 16 hex characters"`
}

// PlanListInput is the input to phelix_plan_list.
type PlanListInput struct {
	App   string `json:"app,omitempty" jsonschema:"filter plans by application name"`
	Limit int    `json:"limit,omitempty" jsonschema:"max plans to list; 0 selects the default (20), capped at 100"`
}

// OperationStatusInput is the input to phelix_operation_status.
type OperationStatusInput struct {
	OperationID string `json:"operation_id" jsonschema:"operation id to query: op_, wh_, mx_ or dep- prefixed"`
}

// PlanApplyInput is the input to phelix_plan_apply.
type PlanApplyInput struct {
	PlanID string `json:"plan_id" jsonschema:"plan id to apply: pln_ followed by 16 hex characters"`
}

// PlanCreateInput is the input to phelix_plan_create. Per-action fields are
// ignored for the other action. Filesystem/build-arg inputs are intentionally
// omitted from the MCP surface (see docs/guides/mcp.md, Limitations).
type PlanCreateInput struct {
	Action string `json:"action" jsonschema:"plan action: rebuild or rollback"`
	App    string `json:"app" jsonschema:"application id or name the plan targets"`

	// Rebuild inputs (deployment-shaping only).
	Port         int    `json:"port,omitempty" jsonschema:"rebuild: public port, same semantics as phelix rebuild --port"`
	Strategy     string `json:"strategy,omitempty" jsonschema:"rebuild: strategy override — classic, blue-green, rolling, canary or progressive"`
	BlueGreen    bool   `json:"blue_green,omitempty" jsonschema:"rebuild: zero-downtime blue-green deployment"`
	Replicas     int    `json:"replicas,omitempty" jsonschema:"rebuild: rolling deployment replica count"`
	Canary       int    `json:"canary,omitempty" jsonschema:"rebuild: canary rollout percentage"`
	Tag          string `json:"tag,omitempty" jsonschema:"rebuild: optional build tag label"`
	AutoRollback bool   `json:"auto_rollback,omitempty" jsonschema:"rebuild: auto-restore the previous version on deploy-phase failure"`
	NoUpload     bool   `json:"no_upload,omitempty" jsonschema:"rebuild: do not upload app information to the backend"`

	// Rollback inputs.
	To     string `json:"to,omitempty" jsonschema:"rollback: target version (e.g. v3, 3, or a tag name)"`
	Verify string `json:"verify,omitempty" jsonschema:"rollback: post-rollback verify duration (e.g. 30s)"`
	Reason string `json:"reason,omitempty" jsonschema:"rollback: reason stored with the plan"`
}

// Services is the seam the MCP transport calls. It is implemented in package
// cmd (which owns the real command logic); internal/mcp never imports cmd, so
// the dependency runs one way only. Every method returns the exact
// machine.Envelope the equivalent CLI command would emit (a success OR a
// Phelix failure envelope); a non-nil error is reserved for an internal
// transport-side failure that has no envelope.
type Services interface {
	Context(ctx context.Context, in ContextInput) (*machine.Envelope, error)
	Inspect(ctx context.Context, in InspectInput) (*machine.Envelope, error)
	PlanShow(ctx context.Context, in PlanShowInput) (*machine.Envelope, error)
	PlanList(ctx context.Context, in PlanListInput) (*machine.Envelope, error)
	OperationStatus(ctx context.Context, in OperationStatusInput) (*machine.Envelope, error)
	PlanCreate(ctx context.Context, in PlanCreateInput) (*machine.Envelope, error)
	PlanApply(ctx context.Context, in PlanApplyInput) (*machine.Envelope, error)
}
