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
	"sync"
	"syscall"

	"github.com/abdorrahmani/phelix/internal/authz"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	phelixmcp "github.com/abdorrahmani/phelix/internal/mcp"
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
var mcpCaptureMu sync.Mutex

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
