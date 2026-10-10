package cmd

// mcp.go — the `phelix mcp serve --stdio` command and the Services
// implementation behind Phelix's MCP adapter (internal/mcp).
//
// This file is the ONLY bridge between the MCP transport and the existing
// command logic. Each Services method runs the exact code path the equivalent
// CLI command runs, under machine.EnterCapture, and returns the machine
// envelope that path produced. There is no second execution model here: plan
// application goes through applyPlan, plan creation through the shared
// buildRebuildSpec/buildRebuildPlan and resolveRollbackTarget/buildRollbackPlan
// path, and the reads through the same Context/Inspect/operation builders.
//
// Authorization: the serve process authorizes as the local, unauthenticated
// MCP caller (authz.LocalMCPAuthenticator), not the CLI. On a host that has
// not configured authorization, MCP-originated plan apply is therefore denied
// until a policy explicitly allows the `mcp` caller — the adapter can never
// gain more privilege than the local CLI, and by default has less.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/abdorrahmani/phelix/internal/authz"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	phelixmcp "github.com/abdorrahmani/phelix/internal/mcp"
	"github.com/abdorrahmani/phelix/internal/session"
	"github.com/spf13/cobra"
)

var mcpServeStdio bool

// MCPCmd is the parent for MCP adapter commands.
var MCPCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Expose Phelix to autonomous coding agents over the Model Context Protocol",
	Long: "Phelix's MCP adapter exposes a small set of existing machine capabilities " +
		"(context/inspection reads, plan create/show/list/apply, operation status) to " +
		"MCP clients over local stdio. It is a transport boundary only — every mutation " +
		"passes the same plan, authorization, approval and idempotency checks as the CLI. " +
		"See docs/guides/mcp.md.",
}

var mcpServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the MCP server over stdio (JSON-RPC on stdout, diagnostics on stderr)",
	Args:  cobra.NoArgs,
	RunE:  runMCPServe,
}

func init() {
	mcpServeCmd.Flags().BoolVar(&mcpServeStdio, "stdio", true, "Serve over stdio (the only supported transport)")
	MCPCmd.AddCommand(mcpServeCmd)
}

// runMCPServe starts the stdio MCP server. It installs the MCP authenticator on
// the process-wide authorization gate, then serves until the client disconnects
// or the process is signalled.
func runMCPServe(cmd *cobra.Command, _ []string) error {
	if !mcpServeStdio {
		return phelixerr.New(phelixerr.CodeInvalidArgument,
			"only --stdio transport is supported; remote/network MCP is out of scope")
	}

	// This process acts for the MCP caller, never the CLI: a distinct,
	// unauthenticated transport that must not inherit legacy-local trust.
	authzGate.Authenticator = authz.LocalMCPAuthenticator{}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Diagnostics only — never stdout (that carries the JSON-RPC protocol).
	fmt.Fprintf(os.Stderr, "phelix mcp: serving on stdio (pid %d)\n", os.Getpid())

	srv := phelixmcp.New(cmdServices{}, phelixmcp.Options{})
	if err := srv.Run(ctx); err != nil {
		if ctx.Err() != nil {
			return nil // signalled / client gone: a clean shutdown
		}
		return phelixerr.Wrap(phelixerr.CodeUnknown, "mcp server terminated", err)
	}
	return nil
}

// cmdServices implements internal/mcp.Services against the real command logic.
type cmdServices struct{}

// mcpCaptureMu serializes captured command execution. machine mode and the
// per-command flag globals are process-wide, so only one captured command may
// run at a time; this also fronts applyPlan's own mutex without reordering.
//
// ponytail: this serializes EVERY tool that runs through captureEnvelope,
// including read-only ones, behind a long phelix_plan_apply (minutes). Reads
// whose command path touches no shared mutable process state (session_show,
// session_list — pure session/plan/ops file-store reads) are served off this
// lock via captureReadEnvelope, so an agent can poll them while its own apply
// runs. The remaining reads (context, inspect, plan_show/list, operation_status)
// stay on this lock because the code they run consults the process-global
// internal/app AppManager and the plan-applicability evaluator, which are NOT
// concurrency-safe (AppManager.LoadState mutates a shared map outside its
// lock). The upgrade path is to make the AppManager read path concurrency-safe
// (or thread the machine envelope writer through a per-call context instead of
// the os.Stdout/envelopeWriter globals) and then route those reads through
// captureReadEnvelope too. See docs/guides/mcp.md (Concurrency).
var mcpCaptureMu sync.Mutex

// captureReadEnvelope serves a read-only tool WITHOUT the process-wide capture
// lock or the os.Stdout swap: it calls build (which must touch no shared
// mutable process state — only idempotent file-store reads) and turns the
// result into the same machine envelope the CLI would emit. Because it never
// mutates machine mode, os.Stdout or any flag global, it is safe to run
// concurrently with a long mutating tool on another connection. The error path
// builds the failure envelope directly rather than through machine.Failure, so
// a concurrent mutation's registered operation id can never leak into a read's
// failure envelope.
func captureReadEnvelope(build func() (any, error)) (*machine.Envelope, error) {
	result, err := build()
	if err != nil {
		return &machine.Envelope{
			SchemaVersion: machine.SchemaVersion,
			Status:        machine.StatusFailed,
			Error:         machine.ErrorBodyFor(err, ExitCodeFor(err), ""),
		}, nil
	}
	return machine.Success("", result), nil
}

// captureEnvelope runs fn — a real command code path — under machine capture
// and returns the machine envelope it produced. fn must leave every `--json`
// flag global false (machine.Active() is already true from capture), so the
// command writes its envelope into the capture buffer instead of stdout. A
// returned error becomes the same failure envelope the CLI error boundary
// would emit, so an authorization denial, stale plan or build failure reaches
// the client as a coded failure envelope, never as success and never dropped.
func captureEnvelope(fn func() error) (*machine.Envelope, error) {
	mcpCaptureMu.Lock()
	defer mcpCaptureMu.Unlock()

	var buf bytes.Buffer
	restore := machine.EnterCapture(&buf)
	err := fn()
	opID := machine.ActiveOperation() // read before restore clears it
	restore()

	if err != nil {
		return machine.Failure(err, ExitCodeFor(err), opID), nil
	}
	if buf.Len() == 0 {
		// Every captured read/mutation path emits an envelope under machine
		// mode; an empty buffer means the wrapper mis-wired a flag global.
		return nil, phelixerr.New(phelixerr.CodeUnknown, "command produced no machine envelope")
	}
	return mcpDecodeEnvelope(buf.Bytes())
}

// mcpDecodeEnvelope parses the single envelope a captured command wrote.
func mcpDecodeEnvelope(data []byte) (*machine.Envelope, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var env machine.Envelope
	if err := dec.Decode(&env); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeUnknown, "decode captured machine envelope", err)
	}
	return &env, nil
}

// appArg returns the positional-arg slice for an optional application id/name.
func appArg(app string) []string {
	if app == "" {
		return nil
	}
	return []string{app}
}

// mcpPlanListLimit bounds the plan-list limit, which internal/plans.List does
// not do on its own (limit <= 0 there means "everything"). Mirrors the
// validateContextLimit contract: 0 -> default, over-cap -> rejected.
func mcpPlanListLimit(v int) (int, error) {
	if v < 0 {
		return 0, phelixerr.Newf(phelixerr.CodeInvalidArgument, "limit must be >= 0 (0 = default %d), got %d", phelixmcp.DefaultPlanList, v)
	}
	if v > phelixmcp.MaxPlanList {
		return 0, phelixerr.Newf(phelixerr.CodeInvalidArgument, "limit must be <= %d, got %d", phelixmcp.MaxPlanList, v)
	}
	if v == 0 {
		return phelixmcp.DefaultPlanList, nil
	}
	return v, nil
}

func (cmdServices) Context(_ context.Context, in phelixmcp.ContextInput) (*machine.Envelope, error) {
	return captureEnvelope(func() error {
		contextJSON = false
		contextVersions = in.Versions
		contextOperations = in.Operations
		contextLogLines = in.LogLines
		return ContextCmd.RunE(ContextCmd, appArg(in.App))
	})
}

func (cmdServices) Inspect(_ context.Context, in phelixmcp.InspectInput) (*machine.Envelope, error) {
	return captureEnvelope(func() error {
		var c *cobra.Command
		var args []string
		switch in.Resource {
		case phelixmcp.ResourceProject:
			c, inspectProjectJSON = inspectProjectCmd, false
		case phelixmcp.ResourceRuntime:
			c, inspectRuntimeJSON = inspectRuntimeCmd, false
		case phelixmcp.ResourceConfig:
			c, inspectConfigJSON = inspectConfigCmd, false
		case phelixmcp.ResourceCapabilities:
			c, inspectCapabilitiesJSON = inspectCapabilitiesCmd, false
		case phelixmcp.ResourceOperations:
			c, inspectOperationsJSON = inspectOperationsCmd, false
			inspectOperationsApp, inspectOperationsLimit = in.App, in.Limit
		case phelixmcp.ResourceApp:
			c, inspectAppJSON, args = inspectAppCmd, false, appArg(in.App)
		case phelixmcp.ResourceDeployment:
			c, inspectDeploymentJSON, args = inspectDeploymentCmd, false, appArg(in.App)
		case phelixmcp.ResourceVersions:
			c, inspectVersionsJSON, args = inspectVersionsCmd, false, appArg(in.App)
			inspectVersionsLimit = in.Limit
		case phelixmcp.ResourceHealth:
			c, inspectHealthJSON, args = inspectHealthCmd, false, appArg(in.App)
		default:
			return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unknown inspect resource %q", in.Resource)
		}
		return c.RunE(c, args)
	})
}

func (cmdServices) PlanShow(_ context.Context, in phelixmcp.PlanShowInput) (*machine.Envelope, error) {
	return captureEnvelope(func() error {
		planShowJSON = false
		return planShowCmd.RunE(planShowCmd, []string{in.PlanID})
	})
}

func (cmdServices) PlanList(_ context.Context, in phelixmcp.PlanListInput) (*machine.Envelope, error) {
	return captureEnvelope(func() error {
		limit, err := mcpPlanListLimit(in.Limit)
		if err != nil {
			return err
		}
		planListJSON = false
		planListApp = in.App
		planListLimit = limit
		return planListCmd.RunE(planListCmd, nil)
	})
}

func (cmdServices) OperationStatus(_ context.Context, in phelixmcp.OperationStatusInput) (*machine.Envelope, error) {
	return captureEnvelope(func() error {
		operationStatusJSON = false
		return operationStatusCmd.RunE(operationStatusCmd, []string{in.OperationID})
	})
}

func (cmdServices) PlanApply(_ context.Context, in phelixmcp.PlanApplyInput) (*machine.Envelope, error) {
	// applyPlan is the one fail-closed application pipeline (load → verify hash
	// → preconditions → capabilities → drift → AUTHORIZE → idempotency →
	// execute → correlate). It validates the id itself, so no second path and
	// no pre-authorization shortcut exist here.
	return captureEnvelope(func() error {
		return applyPlan(in.PlanID)
	})
}

func (cmdServices) PlanCreate(_ context.Context, in phelixmcp.PlanCreateInput) (*machine.Envelope, error) {
	return captureEnvelope(func() error {
		planCreateJSON = false
		switch in.Action {
		case phelixmcp.ActionRebuild:
			planCreatePort = in.Port
			planCreateStrategy = in.Strategy
			planCreateBlueGreen = in.BlueGreen
			planCreateReplicas = in.Replicas
			planCreateCanary = in.Canary
			planCreateTag = in.Tag
			planCreateBuildArgs = nil // build args are not exposed over MCP
			planCreateNoUpload = in.NoUpload
			planCreateAutoRollback = in.AutoRollback
			planCreateSourceDir = "" // source dir is not exposed over MCP
			synth := mcpSyntheticRebuildCreateCmd(in)
			return planCreateRebuildCmd.RunE(synth, appArg(in.App))
		case phelixmcp.ActionRollback:
			planCreateTo = in.To
			planCreateVerify = in.Verify
			planCreateReason = in.Reason
			synth := mcpSyntheticRollbackCreateCmd(in)
			return planCreateRollbackCmd.RunE(synth, appArg(in.App))
		default:
			return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unknown plan action %q", in.Action)
		}
	})
}

// mcpSyntheticRebuildCreateCmd builds a cobra command carrying the rebuild
// create flags set to the MCP inputs and marked Changed, so the shared
// resolver reproduces the CLI's flag precedence exactly (--port explicitness,
// strategy selection). It mirrors syntheticRebuildCommand in plan.go.
func mcpSyntheticRebuildCreateCmd(in phelixmcp.PlanCreateInput) *cobra.Command {
	synth := &cobra.Command{Use: "mcp-plan-create-rebuild"}
	f := synth.Flags()
	f.IntP("port", "p", 8080, "")
	f.String("strategy", "", "")
	f.Bool("blue-green", false, "")
	f.Int("replicas", 0, "")
	f.Int("canary", 0, "")
	f.String("tag", "", "")
	f.StringArrayP("build-arg", "a", nil, "")
	f.Bool("no-upload", false, "")
	f.Bool("auto-rollback", false, "")
	f.String("source-dir", "", "")

	// A port is only "explicitly set" when the client supplied a usable one;
	// port 0 is never valid, so it stands for "unset" and the app's recorded
	// port / phelix.yaml precedence applies instead.
	if in.Port > 0 {
		_ = f.Set("port", fmt.Sprintf("%d", in.Port))
	}
	if in.Strategy != "" {
		_ = f.Set("strategy", in.Strategy)
	}
	if in.BlueGreen {
		_ = f.Set("blue-green", "true")
	}
	if in.Replicas > 0 {
		_ = f.Set("replicas", fmt.Sprintf("%d", in.Replicas))
	}
	if in.Canary > 0 {
		_ = f.Set("canary", fmt.Sprintf("%d", in.Canary))
	}
	if in.Tag != "" {
		_ = f.Set("tag", in.Tag)
	}
	if in.NoUpload {
		_ = f.Set("no-upload", "true")
	}
	if in.AutoRollback {
		_ = f.Set("auto-rollback", "true")
	}
	return synth
}

// mcpSyntheticRollbackCreateCmd builds a cobra command carrying the rollback
// create flags, so the RunE's Changed("reason")/Changed("verify") checks and
// the shared reason/verify validation behave exactly as on the CLI.
func mcpSyntheticRollbackCreateCmd(in phelixmcp.PlanCreateInput) *cobra.Command {
	synth := &cobra.Command{Use: "mcp-plan-create-rollback"}
	f := synth.Flags()
	f.String("to", "", "")
	f.String("verify", "", "")
	f.String("reason", "", "")
	if in.To != "" {
		_ = f.Set("to", in.To)
	}
	if in.Verify != "" {
		_ = f.Set("verify", in.Verify)
	}
	if in.Reason != "" {
		_ = f.Set("reason", in.Reason)
	}
	return synth
}

// mcpSessionListLimit bounds the session-list limit (session.List treats <= 0
// as "everything"); 0 -> default, over-cap -> rejected, mirroring plan list.
func mcpSessionListLimit(v int) (int, error) {
	if v < 0 {
		return 0, phelixerr.Newf(phelixerr.CodeInvalidArgument, "limit must be >= 0 (0 = default %d), got %d", phelixmcp.DefaultSessionList, v)
	}
	if v > phelixmcp.MaxSessionList {
		return 0, phelixerr.Newf(phelixerr.CodeInvalidArgument, "limit must be <= %d, got %d", phelixmcp.MaxSessionList, v)
	}
	if v == 0 {
		return phelixmcp.DefaultSessionList, nil
	}
	return v, nil
}

// The session Services methods drive the real session cobra commands under
// capture, exactly like the plan/context methods. Optimistic-concurrency
// (if-rev) is a CLI-only primitive, so every MCP mutation sets it to -1 (no
// check). The provenance actor is resolved server-side as the mcp caller.
func (cmdServices) SessionCreate(_ context.Context, in phelixmcp.SessionCreateInput) (*machine.Envelope, error) {
	return captureEnvelope(func() error {
		sessionCreateJSON = false
		sessionCreateTitle = in.Title
		sessionCreateApp = in.App
		sessionCreateProject = in.Project
		return sessionCreateCmd.RunE(sessionCreateCmd, nil)
	})
}

func (cmdServices) SessionShow(_ context.Context, in phelixmcp.SessionShowInput) (*machine.Envelope, error) {
	// Read-only and free of shared mutable process state (session + plan/ops
	// file-store reads), so it is served off the capture lock and can run while
	// a long plan_apply is in flight on the same connection.
	return captureReadEnvelope(func() (any, error) {
		return session.Show(strings.TrimSpace(in.SessionID), !in.NoResolve)
	})
}

func (cmdServices) SessionList(_ context.Context, in phelixmcp.SessionListInput) (*machine.Envelope, error) {
	// Read-only, no shared mutable process state — served off the capture lock.
	return captureReadEnvelope(func() (any, error) {
		limit, err := mcpSessionListLimit(in.Limit)
		if err != nil {
			return nil, err
		}
		if in.Status != "" && !session.ValidStatus(in.Status) {
			return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"invalid status %q: expected active, completed, failed or cancelled", in.Status)
		}
		list, _, truncated, err := session.List(in.Status, in.App, limit)
		if err != nil {
			return nil, err
		}
		return session.ListResult{Sessions: list, Count: len(list), Truncated: truncated}, nil
	})
}

func (cmdServices) SessionCheckpoint(_ context.Context, in phelixmcp.SessionCheckpointInput) (*machine.Envelope, error) {
	return captureEnvelope(func() error {
		sessionCheckpointJSON = false
		sessionCheckpointNote = in.Note
		sessionCheckpointStep = in.Step
		sessionCheckpointPlans = in.Plans
		sessionCheckpointOps = in.Operations
		sessionCheckpointDeploys = in.Deployments
		sessionCheckpointIfRev = -1
		return sessionCheckpointCmd.RunE(sessionCheckpointCmd, []string{in.SessionID})
	})
}

func (cmdServices) SessionComplete(_ context.Context, in phelixmcp.SessionCompleteInput) (*machine.Envelope, error) {
	return captureEnvelope(func() error {
		sessionCompleteJSON = false
		sessionCompleteResult = in.Result
		sessionCompletePlans = in.Plans
		sessionCompleteOps = in.Operations
		sessionCompleteDeploys = in.Deployments
		sessionCompleteIfRev = -1
		return sessionCompleteCmd.RunE(sessionCompleteCmd, []string{in.SessionID})
	})
}

func (cmdServices) SessionFail(_ context.Context, in phelixmcp.SessionFailInput) (*machine.Envelope, error) {
	return captureEnvelope(func() error {
		sessionFailJSON = false
		sessionFailReason = in.Reason
		sessionFailPlans = in.Plans
		sessionFailOps = in.Operations
		sessionFailDeploys = in.Deployments
		sessionFailIfRev = -1
		return sessionFailCmd.RunE(sessionFailCmd, []string{in.SessionID})
	})
}
