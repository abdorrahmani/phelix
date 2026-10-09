package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/abdorrahmani/phelix/internal/machine"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTools installs the fixed tool surface. Read tools are marked
// read-only; the two mutation tools carry honest destructive/idempotent hints.
// Hints are advisory only — the real safety is the plan + authorization
// boundary each tool delegates to, never a client's trust in an annotation.
func (s *Server) registerTools() {
	ptr := func(b bool) *bool { return &b }
	readOnly := &mcpsdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}

	mcpsdk.AddTool(s.sdk, &mcpsdk.Tool{
		Name:        ToolContext,
		Title:       "Phelix context",
		Description: "Read a single bounded, structured snapshot of the Phelix environment (project, runtime, config, capabilities and, when an app is named, its deployment/versions/health/operations/logs). Read-only. Returns the Phelix machine envelope.",
		Annotations: readOnly,
	}, s.handleContext)

	mcpsdk.AddTool(s.sdk, &mcpsdk.Tool{
		Name:        ToolInspect,
		Title:       "Phelix inspect",
		Description: "Inspect one resource: " + strings.Join(InspectResources(), ", ") + ". Read-only, structured, bounded. Returns the Phelix machine envelope.",
		Annotations: readOnly,
	}, s.handleInspect)

	mcpsdk.AddTool(s.sdk, &mcpsdk.Tool{
		Name:        ToolPlanShow,
		Title:       "Phelix plan show",
		Description: "Load an immutable plan by id and report it plus its freshly computed applicability. Verifies the plan hash and fails closed on corruption/tampering. Read-only.",
		Annotations: readOnly,
	}, s.handlePlanShow)

	mcpsdk.AddTool(s.sdk, &mcpsdk.Tool{
		Name:        ToolPlanList,
		Title:       "Phelix plan list",
		Description: "List persisted plans (newest first), optionally filtered by application, bounded by limit. Read-only.",
		Annotations: readOnly,
	}, s.handlePlanList)

	mcpsdk.AddTool(s.sdk, &mcpsdk.Tool{
		Name:        ToolOperationStatus,
		Title:       "Phelix operation status",
		Description: "Query one mutation operation by its durable id (op_, wh_, mx_ or dep-) and report its current external state, result and correlation. Read-only.",
		Annotations: readOnly,
	}, s.handleOperationStatus)

	mcpsdk.AddTool(s.sdk, &mcpsdk.Tool{
		Name:        ToolPlanCreate,
		Title:       "Phelix plan create",
		Description: "Create an immutable rebuild or rollback plan from the same resolution the CLI uses. This does NOT execute anything — it produces a plan id and hash to inspect and later apply. Additive (writes only a new plan artifact).",
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), IdempotentHint: false, OpenWorldHint: ptr(false)},
	}, s.handlePlanCreate)

	mcpsdk.AddTool(s.sdk, &mcpsdk.Tool{
		Name:        ToolPlanApply,
		Title:       "Phelix plan apply",
		Description: "Validate a plan against current state and execute it through the existing pipeline: hash verification, staleness and capability checks, the authorization boundary (which may require a plan-bound approval), request-key idempotency, and operation correlation. Fails closed. Repeating with the same plan is idempotent.",
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, s.handlePlanApply)
}

// toolResult renders a machine envelope as the tool's result: exactly one JSON
// document (the envelope) as text content, mirrored into structured content,
// with IsError reflecting a failed Phelix status. A failed envelope is a
// normal, expected result — never collapsed into success, never silently
// dropped.
func toolResult(env *machine.Envelope) (*mcpsdk.CallToolResult, any, error) {
	data, err := machine.MarshalEnvelope(env)
	if err != nil {
		return nil, nil, fmt.Errorf("encode machine envelope: %w", err)
	}
	return &mcpsdk.CallToolResult{
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: string(data)}},
		StructuredContent: json.RawMessage(data),
		IsError:           env != nil && env.Status == machine.StatusFailed,
	}, nil, nil
}

// serviceResult maps a Services call to a tool result. A non-nil error is an
// internal transport-side failure (no envelope); it becomes a tool error.
// Everything a service can express about a Phelix operation — success OR a
// coded failure — travels in the envelope.
func serviceResult(env *machine.Envelope, err error) (*mcpsdk.CallToolResult, any, error) {
	if err != nil {
		return nil, nil, err
	}
	return toolResult(env)
}

// --- handlers -------------------------------------------------------------
//
// Each handler does only transport concerns: validate the enum-shaped inputs
// an input schema cannot express, then delegate to the Services seam and map
// the envelope to a result. No deployment, plan, authorization or state logic
// lives here. The ctx carries the client's cancellation and flows into the
// service.

func (s *Server) handleContext(ctx context.Context, _ *mcpsdk.CallToolRequest, in ContextInput) (*mcpsdk.CallToolResult, any, error) {
	return serviceResult(s.services.Context(ctx, in))
}

func (s *Server) handleInspect(ctx context.Context, _ *mcpsdk.CallToolRequest, in InspectInput) (*mcpsdk.CallToolResult, any, error) {
	// The resource type is an invocation-level enum the input schema cannot
	// constrain, so reject an unknown one as a tool/invocation error rather
	// than engaging any Phelix state.
	if !ValidInspectResource(in.Resource) {
		return nil, nil, fmt.Errorf("unknown resource %q; valid resources are: %s", in.Resource, strings.Join(InspectResources(), ", "))
	}
	return serviceResult(s.services.Inspect(ctx, in))
}

func (s *Server) handlePlanShow(ctx context.Context, _ *mcpsdk.CallToolRequest, in PlanShowInput) (*mcpsdk.CallToolResult, any, error) {
	return serviceResult(s.services.PlanShow(ctx, in))
}

func (s *Server) handlePlanList(ctx context.Context, _ *mcpsdk.CallToolRequest, in PlanListInput) (*mcpsdk.CallToolResult, any, error) {
	return serviceResult(s.services.PlanList(ctx, in))
}

func (s *Server) handleOperationStatus(ctx context.Context, _ *mcpsdk.CallToolRequest, in OperationStatusInput) (*mcpsdk.CallToolResult, any, error) {
	return serviceResult(s.services.OperationStatus(ctx, in))
}

func (s *Server) handlePlanCreate(ctx context.Context, _ *mcpsdk.CallToolRequest, in PlanCreateInput) (*mcpsdk.CallToolResult, any, error) {
	switch in.Action {
	case ActionRebuild, ActionRollback:
	default:
		return nil, nil, fmt.Errorf("unknown action %q; valid actions are: %s, %s", in.Action, ActionRebuild, ActionRollback)
	}
	return serviceResult(s.services.PlanCreate(ctx, in))
}

func (s *Server) handlePlanApply(ctx context.Context, _ *mcpsdk.CallToolRequest, in PlanApplyInput) (*mcpsdk.CallToolResult, any, error) {
	return serviceResult(s.services.PlanApply(ctx, in))
}
