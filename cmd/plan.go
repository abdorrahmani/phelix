package cmd

// plan.go — Phase 3 plans as first-class machine artifacts:
//
//	phelix plan create rebuild|rollback   (same flags as the commands)
//	phelix plan show <plan-id>            (content + fresh applicability)
//	phelix plan list [--app] [--limit]
//	phelix plan apply <plan-id>           (validate → execute → correlate)
//
// Plans are generated from the same resolution semantics the commands use
// (buildRebuildSpec / resolveRollbackTarget / deploy.PlanRollback) and
// applied through the same execution paths (runRebuildExec /
// rollbackClassic / rollbackZeroDowntime). There is no parallel execution
// model anywhere in this file.

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/abdorrahmani/phelix/internal/plans"
	"github.com/spf13/cobra"
)

// planExecRebuild / planExecRollback are the application seams: plan apply
// dispatches through these indirections so mutation-safety tests can assert
// execution_count == 0 without touching a real engine. Production never
// swaps them.
var (
	planExecRebuild  = runRebuildExec
	planExecRollback = execRollbackForPlan
)

// planApplyMu serializes plan application within one process: the resolution
// step reads shared in-process state (the app manager), and cross-process
// duplicate safety is the request-key ledger's job. Within a process, two
// concurrent applies of the same plan must see each other's result, not race
// on resolution.
var planApplyMu sync.Mutex

// planRequestKey derives the idempotency key for a plan application. The
// Phase 1 request-key ledger makes every application of the same plan
// deterministic and duplicate-safe, across restarts included.
func planRequestKey(planID string) string { return "pln:" + planID }

var PlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Create, inspect and apply immutable execution plans (machine-first)",
	Long: "Plans are immutable, content-addressed descriptions of what a mutation " +
		"would execute, with typed preconditions re-evaluated at apply time. " +
		"Use --json for the machine envelope (docs/reference/machine-contract.md).",
}

// --- plan create ---------------------------------------------------------------

var (
	planCreateJSON         bool
	planCreatePort         int
	planCreateStrategy     string
	planCreateBlueGreen    bool
	planCreateReplicas     int
	planCreateCanary       int
	planCreateTag          string
	planCreateBuildArgs    []string
	planCreateNoUpload     bool
	planCreateAutoRollback bool
	planCreateSourceDir    string

	planCreateTo     string
	planCreateVerify string
	planCreateReason string
)

var planCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create an execution plan (rebuild | rollback)",
}

var planCreateRebuildCmd = &cobra.Command{
	Use:   "rebuild [ID|AppName]",
	Short: "Plan a rebuild/deploy of an application (same inputs as phelix rebuild)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if planCreateJSON {
			restore := machine.EnterJSON()
			defer restore()
		}

		// Mirror the rebuild command's flag inputs onto the same globals the
		// shared resolver reads, then resolve through the identical path.
		rebuildPort = planCreatePort
		rebuildStrategy = planCreateStrategy
		rebuildBlueGreen = planCreateBlueGreen
		rebuildReplicas = planCreateReplicas
		rebuildCanary = planCreateCanary
		rebuildTag = planCreateTag
		rebuildArgs = planCreateBuildArgs
		rebuildNoUpload = planCreateNoUpload
		rebuildAutoRollback = planCreateAutoRollback
		rebuildSourceDir = planCreateSourceDir
		rolloutPlan = nil

		spec, err := buildRebuildSpec(cmd, args)
		if err != nil {
			return err
		}

		plan, err := buildRebuildPlan(spec, cmd.Flags().Changed("port"))
		if err != nil {
			return err
		}
		return emitPlanCreated(plan)
	},
}

var planCreateRollbackCmd = &cobra.Command{
	Use:   "rollback [ID|AppName]",
	Short: "Plan a rollback of an application (same inputs as phelix rollback)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if planCreateJSON {
			restore := machine.EnterJSON()
			defer restore()
		}

		appName, err := resolvePlanApp(args)
		if err != nil {
			return err
		}

		// Same validation rules as the rollback command.
		reason, err := deploy.ValidateRollbackReason(planCreateReason, cmd.Flags().Changed("reason"))
		if err != nil {
			return err
		}
		var verifyDuration time.Duration
		if cmd.Flags().Changed("verify") {
			d, parseErr := time.ParseDuration(planCreateVerify)
			if parseErr != nil {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument,
					"invalid --verify duration %q: use a Go duration like 30s, 1m or 2m30s", planCreateVerify)
			}
			if d <= 0 {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "invalid --verify duration %q: must be positive", planCreateVerify)
			}
			verifyDuration = d
		}

		// Shared target resolution (the same path the rollback command and
		// the remote rollback use), including the rollback-to-current guard.
		target, targetTag, targetSource, err := resolveRollbackTarget(appName, planCreateTo)
		if err != nil {
			return err
		}

		plan, err := buildRollbackPlan(appName, target, targetTag, targetSource, reason, verifyDuration)
		if err != nil {
			return err
		}
		return emitPlanCreated(plan)
	},
}

func resolvePlanApp(args []string) (string, error) {
	if err := app.Manager.LoadState(); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
	}
	if len(args) == 0 || args[0] == "" {
		return "", phelixerr.New(phelixerr.CodeInvalidArgument, "missing required <ID|AppName>; usage: phelix plan create <action> <ID|AppName>")
	}
	appInfo, err := GetAppInfo(args[0])
	if err != nil {
		return "", err
	}
	return appInfo.Name, nil
}

// buildRebuildPlan assembles the plan from a resolved rebuild spec and the
// current state. Preconditions pin exactly the facts execution depends on.
func buildRebuildPlan(spec *rebuildSpec, portExplicit bool) (*plans.Plan, error) {
	fingerprint, err := plans.RebuildConfigFingerprint(spec.Name, spec.SourceDir)
	if err != nil {
		return nil, err
	}
	commit := deploy.DetectGitCommit(spec.SourceDir)

	currentVersion := "none"
	if cur, curErr := deploy.CurrentVersion(spec.Name); curErr == nil && cur > 0 {
		currentVersion = fmt.Sprintf("v%d", cur)
	}

	steps := []string{
		"build source (" + string(spec.Lang) + ")",
	}
	execution := plans.Execution{
		Strategy:   spec.Strategy,
		Replicas:   spec.Replicas,
		PublicPort: spec.Port,
	}
	if spec.Strategy == "blue-green" || spec.Strategy == "rolling" || spec.Strategy == "canary" || spec.Strategy == "progressive" {
		steps = append(steps,
			"start candidate instance on internal port",
			"run tiered health checks",
			"switch proxy target (zero downtime)",
			"drain and stop the old instance",
		)
		execution.Runtime = deployRuntimeForPlan(spec.SourceDir)
		execution.Network = deployNetworkForPlan(spec.SourceDir)
		if spec.Strategy == "rolling" {
			steps = append(steps, "repeat per replica")
		}
	} else {
		steps = append(steps,
			"stop the current instance",
			"start the new binary on the public port",
			"promote the recorded version",
		)
	}

	plan := &plans.Plan{
		Status: plans.StatusCreated,
		Action: plans.Action{Type: plans.ActionRebuild, Application: spec.Name},
		Target: plans.Target{
			AppID:    spec.AppInfo.ID,
			AppName:  spec.Name,
			Language: string(spec.Lang),
		},
		Inputs: plans.Inputs{
			SourceDir:         spec.SourceDir,
			Port:              spec.Port,
			PortExplicit:      portExplicit,
			Tag:               planCreateTag,
			BuildArgs:         append([]string(nil), planCreateBuildArgs...),
			AutoRollback:      planCreateAutoRollback,
			NoUpload:          planCreateNoUpload,
			StrategyOverride:  planCreateStrategy,
			FlagBlueGreen:     planCreateBlueGreen,
			FlagReplicas:      planCreateReplicas,
			FlagCanary:        planCreateCanary,
			SourceCommit:      commit,
			ConfigFingerprint: fingerprint,
		},
		Execution: execution,
		Preconditions: []plans.Precondition{
			{Type: plans.PreconditionAppExists, Expected: spec.AppInfo.ID, Source: "apps.json"},
			{Type: plans.PreconditionCurrentVersion, Expected: currentVersion, Source: "versions.json"},
			{Type: plans.PreconditionConfigMatch, Expected: fingerprint, Source: "phelix.yaml + deploy.json"},
			{Type: plans.PreconditionToolchain, Expected: "installed", Source: "toolchain"},
		},
		Capabilities: []string{phelixgrpc.CapabilityDeployment},
	}
	if commit != "" {
		plan.Preconditions = append(plan.Preconditions,
			plans.Precondition{Type: plans.PreconditionSourceCommit, Expected: commit, Source: "git"})
	}
	if err := plan.Finalize(); err != nil {
		return nil, err
	}
	return plan, nil
}

// buildRollbackPlan assembles a rollback plan from the shared target
// resolution and the same planning helper `rollback --dry-run` uses.
func buildRollbackPlan(appName string, target int, targetTag, targetSource, reason string, verifyDuration time.Duration) (*plans.Plan, error) {
	appInfo, err := GetAppInfo(appName)
	if err != nil {
		return nil, err
	}

	// The rollback plan reuses deploy.PlanRollback — the authoritative
	// read-only execution description (same helper, same validation).
	port := appInfo.Port
	if port == 0 {
		port = defaultPort
	}
	rollbackPlan, err := deploy.PlanRollback(appName, deploy.RollbackPlanInput{
		AppID:          appInfo.ID,
		Target:         target,
		ClassicRunning: appInfo.Status == "running",
		ClassicPort:    port,
		ClassicDestBin: appInfo.Directory + string(os.PathSeparator) + fmt.Sprintf("app_%s", appInfo.ID),
	})
	if err != nil {
		return nil, err
	}

	mode := "classic"
	if rollbackPlan.Strategy != "" {
		mode = rollbackPlan.Strategy
	}
	currentVersion := "none"
	if rollbackPlan.CurrentVersion > 0 {
		currentVersion = fmt.Sprintf("v%d", rollbackPlan.CurrentVersion)
	}

	plan := &plans.Plan{
		Status: plans.StatusCreated,
		Action: plans.Action{Type: plans.ActionRollback, Application: appName},
		Target: plans.Target{
			AppID:    appInfo.ID,
			AppName:  appName,
			Language: appInfo.Language,
		},
		Inputs: plans.Inputs{
			TargetVersion: target,
			TargetTag:     targetTag,
			VerifyMs:      verifyDuration.Milliseconds(),
			Reason:        reason,
		},
		Execution: plans.Execution{
			Strategy:             mode,
			PublicPort:           rollbackPlan.PublicPort,
			CurrentSlot:          rollbackPlan.CurrentSlot,
			TargetSlot:           rollbackPlan.TargetSlot,
			FromVersion:          rollbackPlan.CurrentVersion,
			ToVersion:            rollbackPlan.TargetVersion,
			Downtime:             rollbackPlan.Downtime,
			EnvSnapshotAvailable: rollbackPlan.EnvSnapshotAvailable,
			Steps:                append([]string(nil), rollbackPlan.Steps...),
		},
		Preconditions: []plans.Precondition{
			{Type: plans.PreconditionAppExists, Expected: appInfo.ID, Source: "apps.json"},
			{Type: plans.PreconditionVersionExists, Expected: versionLabelOf(target), Source: "versions.json"},
			{Type: plans.PreconditionCurrentVersion, Expected: currentVersion, Source: "versions.json"},
			{Type: plans.PreconditionDeployMode, Expected: mode, Source: "deploy.json"},
		},
		Capabilities: []string{phelixgrpc.CapabilityRollback},
	}
	if err := plan.Finalize(); err != nil {
		return nil, err
	}
	return plan, nil
}

func versionLabelOf(v int) string {
	if v <= 0 {
		return "none"
	}
	return fmt.Sprintf("v%d", v)
}

// deployRuntimeForPlan / deployNetworkForPlan read the resolved docker
// runtime fields for the execution description.
func deployRuntimeForPlan(sourceDir string) string {
	cfg, err := loadProjectConfigFrom(sourceDir)
	if err != nil || cfg == nil {
		return "native"
	}
	r := cfg.DeployRuntime()
	if r == "" {
		return "native"
	}
	return r
}

func deployNetworkForPlan(sourceDir string) string {
	cfg, err := loadProjectConfigFrom(sourceDir)
	if err != nil || cfg == nil {
		return ""
	}
	return cfg.DeployNetwork()
}

// emitPlanCreated persists the plan (immutably) and renders the result.
func emitPlanCreated(plan *plans.Plan) error {
	if err := plans.Save(plan); err != nil {
		return err
	}
	if machine.Active() {
		return writeEnvelopeResult(machine.Success("", &planView{Plan: plan, Applicability: computeApplicability(plan)}))
	}
	printPlanSummary(plan, computeApplicability(plan))
	return nil
}

// --- plan show ------------------------------------------------------------------

var planShowJSON bool

var planShowCmd = &cobra.Command{
	Use:   "show <plan-id>",
	Short: "Show a plan and whether it is currently applicable",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if planShowJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		plan, err := plans.Load(args[0])
		if err != nil {
			return err
		}
		applicability := computeApplicability(plan)
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", &planView{Plan: plan, Applicability: applicability}))
		}
		printPlanDetail(plan, applicability)
		return nil
	},
}

// planApplicability is the FRESH applicability of a stored plan, computed
// from current execution-relevant state — never trusted from the stored
// status field.
type planApplicability struct {
	State               string                     `json:"state"` // applicable | stale
	CheckedAtMs         int64                      `json:"checked_at_ms"`
	FailedPreconditions []plans.FailedPrecondition `json:"failed_preconditions,omitempty"`
	MissingCapabilities []string                   `json:"missing_capabilities,omitempty"`
}

func computeApplicability(plan *plans.Plan) *planApplicability {
	app := &planApplicability{
		State:       "applicable",
		CheckedAtMs: time.Now().UnixMilli(),
	}
	app.FailedPreconditions = plan.Evaluate()
	app.MissingCapabilities = plans.EvaluateCapabilities(plan, phelixgrpc.AgentCapabilities())
	if len(app.FailedPreconditions) > 0 || len(app.MissingCapabilities) > 0 {
		app.State = "stale"
	}
	return app
}

// planView is the machine representation of a plan plus its fresh
// applicability.
type planView struct {
	*plans.Plan
	Applicability *planApplicability `json:"applicability"`
}

// --- plan list ---------------------------------------------------------------------

var (
	planListJSON  bool
	planListApp   string
	planListLimit int
)

var planListCmd = &cobra.Command{
	Use:   "list",
	Short: "List persisted plans (newest first)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if planListJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		if planListLimit < 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "--limit must be >= 0 (0 = all)")
		}
		list, skipped, err := plans.List(planListApp, planListLimit)
		if err != nil {
			return err
		}
		if skipped > 0 {
			logs.WarningFile("plans", "%d corrupt plan file(s) skipped", skipped)
		}
		views := make([]*planView, 0, len(list))
		for _, p := range list {
			views = append(views, &planView{Plan: p, Applicability: computeApplicability(p)})
		}
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", planListResult{Plans: views, Count: len(views)}))
		}
		if len(views) == 0 {
			fmt.Println("No plans recorded. Create one with 'phelix plan create rebuild|rollback'.")
			return nil
		}
		for _, v := range views {
			fmt.Printf("%s  %-9s %-12s %-10s %s\n", v.PlanID, v.Action.Type, v.Action.Application, v.Status, v.PlanHash)
		}
		return nil
	},
}

type planListResult struct {
	Plans []*planView `json:"plans"`
	Count int         `json:"count"`
}

// --- plan apply ---------------------------------------------------------------------

var planApplyJSON bool

var planApplyCmd = &cobra.Command{
	Use:   "apply <plan-id>",
	Short: "Validate a plan against current state and execute it (fails closed)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if planApplyJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		return applyPlan(args[0])
	},
}

// applyPlan is the fail-closed application pipeline. NOTHING mutates before
// every validation step has passed AND the authorization boundary has allowed
// the execution; execution dispatches through the seam variables (the real
// engine in production, a counting stub in tests).
//
// The order is the Phase 4 security contract:
//
//	load plan → verify hash → preconditions → capabilities →
//	resolve execution spec → verify plan/spec equivalence →
//	AUTHORIZE → idempotency → execute → correlate
//
// Authorization is deliberately the last gate before mutation (so the
// decision cannot go stale before it is acted on) and deliberately BEFORE the
// request-key ledger (so a refusal consumes no idempotency key — the same
// plan applies cleanly once it is approved).
func applyPlan(planID string) (err error) {
	planApplyMu.Lock()
	defer planApplyMu.Unlock()

	plan, err := plans.Load(planID)
	if err != nil {
		return err
	}

	// An already-applied plan is never executed twice: return the recorded
	// operation correlation instead (documented repeat-apply semantics). This
	// precedes authorization on purpose — it is a read of durable correlation
	// with zero mutation by construction, and Phase 3's repeat-apply contract
	// must not change meaning because a policy or approval was edited after
	// the execution that already happened.
	if plan.Status == plans.StatusApplied {
		return emitAlreadyApplied(plan)
	}

	// Staleness and capability validation happen BEFORE any execution input
	// is touched. A stale plan is never re-planned, never substituted, never
	// partially executed.
	if failures := plan.Evaluate(); len(failures) > 0 {
		return phelixerr.New(phelixerr.CodePlanStale, describeStaleness(failures, nil))
	}
	if missing := plans.EvaluateCapabilities(plan, phelixgrpc.AgentCapabilities()); len(missing) > 0 {
		return phelixerr.Newf(phelixerr.CodePlanCapabilityMissing,
			"plan requires capabilities this build does not provide: %s", strings.Join(missing, ", "))
	}

	switch plan.Action.Type {
	case plans.ActionRebuild:
		// Restore the plan's execution inputs onto the rebuild flag globals
		// and re-resolve through the shared spec builder, then require the
		// resolution to reproduce the plan bit-for-bit before mutating.
		synthetic := syntheticRebuildCommand(plan)
		rebuildPort = plan.Inputs.Port
		rebuildStrategy = plan.Inputs.StrategyOverride
		rebuildTag = plan.Inputs.Tag
		rebuildArgs = append([]string(nil), plan.Inputs.BuildArgs...)
		rebuildNoUpload = plan.Inputs.NoUpload
		rebuildAutoRollback = plan.Inputs.AutoRollback
		rebuildSourceDir = plan.Inputs.SourceDir
		rebuildCanary = plan.Inputs.FlagCanary
		rolloutPlan = nil

		spec, specErr := buildRebuildSpec(synthetic, []string{plan.Action.Application})
		if specErr != nil {
			return specErr
		}
		if drift := compareSpecToPlan(spec, plan); drift != "" {
			return phelixerr.Newf(phelixerr.CodePlanStale,
				"resolved execution drifted from the plan (%s); create a new plan", drift)
		}

		// The authorization boundary. Last gate before mutation: no operation
		// record, no ledger entry and no engine call exists yet, so a refusal
		// here is provably a zero-mutation outcome.
		authzOutcome, authzErr := authorizePlanExecution(plan)
		if authzErr != nil {
			return authzErr
		}

		op := newOpRun(ops.KindRebuild, spec.Name, planRequestKey(plan.PlanID))
		defer func() { op.finish(&err) }()
		replayed, idemErr := op.beginIdempotency(map[string]string{
			"plan_hash": plan.PlanHash,
		})
		if idemErr != nil {
			return idemErr
		}
		if replayed {
			// The ledger replayed the terminal envelope of a previous
			// application (success or failure) — deterministic, no mutation.
			return nil
		}
		// The operation record exists only after beginIdempotency (a replayed
		// plan must not mint one), so correlation is stamped here.
		op.setPlanCorrelation(plan.PlanID, plan.PlanHash)
		op.setAuthzCorrelation(authzOutcome.DecisionID, authzOutcome.Decision.ApprovalID)

		execErr := planExecRebuild(spec, op)
		recordPlanTerminal(plan, op, execErr)
		return execErr

	case plans.ActionRollback:
		// Same boundary, same position: the rollback path re-resolves nothing
		// before execution, so the gate sits immediately before the operation
		// record and the ledger.
		authzOutcome, authzErr := authorizePlanExecution(plan)
		if authzErr != nil {
			return authzErr
		}

		op := newOpRun(ops.KindRollback, plan.Action.Application, planRequestKey(plan.PlanID))
		defer func() { op.finish(&err) }()
		replayed, idemErr := op.beginIdempotency(map[string]string{
			"plan_hash": plan.PlanHash,
		})
		if idemErr != nil {
			return idemErr
		}
		if replayed {
			return nil
		}
		// Correlation is stamped after the record exists (see rebuild branch).
		op.setPlanCorrelation(plan.PlanID, plan.PlanHash)
		op.setAuthzCorrelation(authzOutcome.DecisionID, authzOutcome.Decision.ApprovalID)

		execErr := planExecRollback(plan, op)
		recordPlanTerminal(plan, op, execErr)
		return execErr

	default:
		return phelixerr.Newf(phelixerr.CodePlanInvalid, "unsupported plan action %q", plan.Action.Type)
	}
}

// recordPlanTerminal records the plan's terminal status after an application
// attempt. Best-effort: a correlation write failure must not turn a
// successful execution into a failed command (the operation record and the
// idempotency ledger still carry the outcome).
func recordPlanTerminal(plan *plans.Plan, op *opRun, execErr error) {
	if execErr != nil {
		_, _ = plans.MarkFailed(plan.PlanID)
		return
	}
	deploymentID := ""
	if op != nil && op.rec != nil {
		deploymentID = op.rec.DeploymentID
	}
	_, _ = plans.MarkApplied(plan.PlanID, op.operationID(), deploymentID)
}

// describeStaleness renders the failed preconditions into a redacted,
// structured message.
func describeStaleness(failures []plans.FailedPrecondition, missing []string) string {
	var b strings.Builder
	b.WriteString("plan is stale — current state no longer matches what was planned; create a new plan")
	for _, f := range failures {
		fmt.Fprintf(&b, "\n  %s: expected %s, actual %s", f.Type, f.Expected, phelixerr.Redact(f.Actual))
	}
	for _, m := range missing {
		fmt.Fprintf(&b, "\n  capability missing: %s", m)
	}
	return b.String()
}

// emitAlreadyApplied returns the recorded correlation for a plan that already
// produced an operation. No mutation, no re-execution.
func emitAlreadyApplied(plan *plans.Plan) error {
	result := map[string]any{
		"plan_id":         plan.PlanID,
		"plan_hash":       plan.PlanHash,
		"already_applied": true,
	}
	if plan.Correlation != nil {
		result["operation_id"] = plan.Correlation.OperationID
		result["deployment_id"] = plan.Correlation.DeploymentID
	}
	if machine.Active() {
		return writeEnvelopeResult(machine.Success(plan.CorrelationOperationID(), result))
	}
	fmt.Printf("Plan %s was already applied (operation %s). No mutation performed.\n",
		plan.PlanID, plan.CorrelationOperationID())
	return nil
}

// syntheticRebuildCommand builds an internal cobra command carrying rebuild's
// flags, set to the plan's captured inputs and marked Changed, so the shared
// resolver reproduces the original flag precedence exactly. It is never
// printed or user-visible.
func syntheticRebuildCommand(plan *plans.Plan) *cobra.Command {
	synth := &cobra.Command{Use: "plan-apply-rebuild"}
	flags := synth.Flags()
	flags.IntP("port", "p", 8080, "")
	flags.String("strategy", "", "")
	flags.Bool("blue-green", false, "")
	flags.Int("replicas", 0, "")
	flags.Int("canary", 0, "")
	flags.String("tag", "", "")
	flags.StringArrayP("build-arg", "a", nil, "")
	flags.Bool("no-upload", false, "")
	flags.Bool("auto-rollback", false, "")
	flags.String("source-dir", "", "")

	if plan.Inputs.PortExplicit {
		_ = flags.Set("port", fmt.Sprintf("%d", plan.Inputs.Port))
	}
	if plan.Inputs.StrategyOverride != "" {
		_ = flags.Set("strategy", plan.Inputs.StrategyOverride)
	}
	if plan.Inputs.FlagBlueGreen {
		_ = flags.Set("blue-green", "true")
	}
	if plan.Inputs.FlagReplicas > 0 {
		_ = flags.Set("replicas", fmt.Sprintf("%d", plan.Inputs.FlagReplicas))
	}
	if plan.Inputs.FlagCanary > 0 {
		_ = flags.Set("canary", fmt.Sprintf("%d", plan.Inputs.FlagCanary))
	}
	if plan.Inputs.Tag != "" {
		_ = flags.Set("tag", plan.Inputs.Tag)
	}
	for _, arg := range plan.Inputs.BuildArgs {
		_ = flags.Set("build-arg", arg)
	}
	if plan.Inputs.NoUpload {
		_ = flags.Set("no-upload", "true")
	}
	if plan.Inputs.AutoRollback {
		_ = flags.Set("auto-rollback", "true")
	}
	if plan.Inputs.SourceDir != "" {
		_ = flags.Set("source-dir", plan.Inputs.SourceDir)
	}
	return synth
}

// compareSpecToPlan verifies that re-resolution reproduced the plan. Any
// drift means execution would differ from what was planned.
func compareSpecToPlan(spec *rebuildSpec, plan *plans.Plan) string {
	switch {
	case spec.Port != plan.Inputs.Port:
		return fmt.Sprintf("port resolved to %d, planned %d", spec.Port, plan.Inputs.Port)
	case spec.Strategy != plan.Execution.Strategy:
		return fmt.Sprintf("strategy resolved to %q, planned %q", spec.Strategy, plan.Execution.Strategy)
	case spec.Strategy == "rolling" && spec.Replicas != plan.Execution.Replicas:
		return fmt.Sprintf("replicas resolved to %d, planned %d", spec.Replicas, plan.Execution.Replicas)
	case spec.SourceDir != plan.Inputs.SourceDir:
		return fmt.Sprintf("source dir resolved to %q, planned %q", spec.SourceDir, plan.Inputs.SourceDir)
	case string(spec.Lang) != plan.Target.Language:
		return fmt.Sprintf("language resolved to %q, planned %q", spec.Lang, plan.Target.Language)
	default:
		return ""
	}
}

// execRollbackForPlan executes a rollback plan through the existing rollback
// engine (the same functions `phelix rollback` runs), with the plan's frozen
// target and the operation as the event correlation ID.
func execRollbackForPlan(plan *plans.Plan, op *opRun) error {
	appInfo, err := GetAppInfo(plan.Action.Application)
	if err != nil {
		return err
	}
	verifyDuration := time.Duration(plan.Inputs.VerifyMs) * time.Millisecond
	requestID := op.requestIdForEvents()

	state, classifyErr := classifyRollbackDeployState(plan.Action.Application)
	if classifyErr != nil {
		return classifyErr
	}
	if state == nil {
		return rollbackClassic(appInfo, plan.Action.Application, plan.Inputs.TargetVersion,
			plan.Inputs.TargetTag, rollbackTargetExplicit, plan.Inputs.Reason, verifyDuration, requestID, op)
	}
	return rollbackZeroDowntime(appInfo, plan.Action.Application, plan.Inputs.TargetVersion,
		plan.Inputs.TargetTag, rollbackTargetExplicit, state, plan.Inputs.Reason, verifyDuration, requestID, op)
}

// --- human rendering -----------------------------------------------------------

func printPlanSummary(plan *plans.Plan, app *planApplicability) {
	printPlanDetail(plan, app)
}

func printPlanDetail(plan *plans.Plan, app *planApplicability) {
	fmt.Printf("Plan:      %s\n", plan.PlanID)
	fmt.Printf("Action:    %s\n", plan.Action.Type)
	fmt.Printf("Application: %s\n", plan.Action.Application)
	if plan.Execution.Strategy != "" {
		fmt.Printf("Strategy:  %s\n", plan.Execution.Strategy)
	}
	if plan.Inputs.Port != 0 {
		fmt.Printf("Port:      %d\n", plan.Inputs.Port)
	}
	if plan.Inputs.TargetVersion != 0 {
		fmt.Printf("Target:    v%d %s\n", plan.Inputs.TargetVersion, plan.Inputs.TargetTag)
	}
	fmt.Printf("Hash:      %s\n", plan.PlanHash)
	fmt.Printf("Status:    %s\n", plan.Status)
	fmt.Println("Preconditions:")
	for _, pre := range plan.Preconditions {
		marker := "✓"
		for _, f := range app.FailedPreconditions {
			if f.Type == pre.Type {
				marker = "✗"
			}
		}
		fmt.Printf("  %s %s (expected %s)\n", marker, pre.Type, pre.Expected)
	}
	fmt.Printf("Applicable: %s\n", app.State)
	if plan.Correlation != nil {
		fmt.Printf("Operation: %s\n", plan.Correlation.OperationID)
	}
}

func init() {
	planCreateRebuildCmd.Flags().IntVarP(&planCreatePort, "port", "p", 8080, "Port to run the application on (same semantics as phelix rebuild)")
	planCreateRebuildCmd.Flags().StringVar(&planCreateStrategy, "strategy", "", "Deployment strategy for this rebuild only: classic, blue-green, rolling, canary, or progressive")
	planCreateRebuildCmd.Flags().BoolVar(&planCreateBlueGreen, "blue-green", false, "Zero-downtime blue-green deployment")
	planCreateRebuildCmd.Flags().IntVar(&planCreateReplicas, "replicas", 0, "Zero-downtime rolling deployment over N replicas")
	planCreateRebuildCmd.Flags().IntVar(&planCreateCanary, "canary", 0, "Canary rollout at N percent of traffic")
	planCreateRebuildCmd.Flags().StringVar(&planCreateTag, "tag", "", "Optional tag for the build this plan will produce")
	planCreateRebuildCmd.Flags().StringArrayVarP(&planCreateBuildArgs, "build-arg", "a", nil, "Extra build argument (repeatable)")
	planCreateRebuildCmd.Flags().BoolVar(&planCreateNoUpload, "no-upload", false, "Do not upload app information to the server")
	planCreateRebuildCmd.Flags().BoolVar(&planCreateAutoRollback, "auto-rollback", false, "Automatically restore the previous version on deploy-phase failure")
	planCreateRebuildCmd.Flags().StringVar(&planCreateSourceDir, "source-dir", "", "Build from this source directory instead of the app's directory")

	planCreateRollbackCmd.Flags().StringVar(&planCreateTo, "to", "", "Roll back to a specific version (e.g. v3, 3, or a tag name)")
	planCreateRollbackCmd.Flags().StringVar(&planCreateVerify, "verify", "", "Verify duration after the rollback (e.g. 30s)")
	planCreateRollbackCmd.Flags().StringVar(&planCreateReason, "reason", "", "Why this rollback is planned (stored with the plan)")

	planShowCmd.Flags().BoolVar(&planShowJSON, "json", false, "Output the machine envelope on stdout")
	planListCmd.Flags().BoolVar(&planListJSON, "json", false, "Output the machine envelope on stdout")
	planListCmd.Flags().StringVar(&planListApp, "app", "", "Filter plans by application")
	planListCmd.Flags().IntVar(&planListLimit, "limit", 0, "Maximum plans to list (0 = all)")
	planApplyCmd.Flags().BoolVar(&planApplyJSON, "json", false, "Output the machine envelope on stdout")

	planCreateCmd.AddCommand(planCreateRebuildCmd)
	planCreateCmd.AddCommand(planCreateRollbackCmd)
	PlanCmd.AddCommand(planCreateCmd)
	PlanCmd.AddCommand(planShowCmd)
	PlanCmd.AddCommand(planListCmd)
	PlanCmd.AddCommand(planApplyCmd)
}

// Both create subcommands share the --json flag variable; cobra requires
// per-command registration.
func init() {
	planCreateRebuildCmd.Flags().BoolVar(&planCreateJSON, "json", false, "Output the machine envelope on stdout")
	planCreateRollbackCmd.Flags().BoolVar(&planCreateJSON, "json", false, "Output the machine envelope on stdout")
}
