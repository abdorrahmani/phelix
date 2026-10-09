package cmd

// authz.go — the minimal CLI surface over the execution authorization
// boundary.
//
//	phelix authz status              — what posture is this host in?
//	phelix authz check <plan-id>     — what would the boundary answer? (no mutation)
//	phelix authz approve <plan-id>   — bind an approval to this exact plan
//	phelix authz revoke <plan-id>    — remove that approval
//
// Authorization is an execution boundary, not a command a user runs before
// every deploy: the normal workflow stays `plan create` → `plan show` →
// `plan apply`, and these four exist for inspecting the boundary and for the
// one act that genuinely needs a human decision.
//
// There is deliberately no command that writes rules. The host policy is a
// file an operator owns (see `phelix authz status` for its path and the
// authorization guide for its schema) — the same shape as every other
// host-level trust decision, and not something a deploying agent should be
// able to rewrite through a convenience command.

import (
	"context"
	"fmt"
	"time"

	"github.com/abdorrahmani/phelix/internal/authz"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/plans"
	"github.com/spf13/cobra"
)

var AuthzCmd = &cobra.Command{
	Use:   "authz",
	Short: "Inspect the execution authorization boundary and approve plans",
	Long: "Phelix authorizes execution at a single boundary between a validated plan " +
		"and the execution engine. These commands report what that boundary would " +
		"decide and record approvals bound to one exact plan. " +
		"Use --json for the machine envelope (docs/reference/machine-contract.md).",
}

// --- authz status ------------------------------------------------------------

var authzStatusJSON bool

// authzStatusResult reports the host's authorization posture. It contains no
// secrets: a policy is a rule set over actor types, action names and
// application names.
type authzStatusResult struct {
	Mode string `json:"mode"`
	// PolicyPath is where the policy file is read from, whether or not it
	// exists — an operator needs the path to create one.
	PolicyPath   string `json:"policy_path"`
	PolicyLoaded bool   `json:"policy_loaded"`
	// PolicyError is the structured reason the policy could not be used.
	// While it is set, every protected execution fails closed.
	PolicyError     *authzStatusError `json:"policy_error,omitempty"`
	Enforced        bool              `json:"enforced"`
	Rules           int               `json:"rules"`
	ApprovalsDir    string            `json:"approvals_dir"`
	DecisionLogPath string            `json:"decision_log_path"`
	// Actor is how this invocation is seen by the boundary. It is always the
	// unauthenticated local CLI actor in this release: Phelix accepts no flag
	// or environment value that can change it.
	Actor *machine.Actor `json:"actor"`
}

type authzStatusError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var authzStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show this host's authorization posture",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if authzStatusJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		actor, err := authz.LocalCLIAuthenticator{}.Authenticate()
		if err != nil {
			return err
		}
		res := &authzStatusResult{
			PolicyPath:      authz.PolicyPath(),
			ApprovalsDir:    authz.ApprovalsDir(),
			DecisionLogPath: authz.DecisionsPath(),
			Actor:           actor.Machine(),
		}
		policy, loadErr := authz.LoadPolicy()
		switch {
		case loadErr != nil:
			// A broken policy is reported, not hidden: this is the state in
			// which every protected execution is failing closed.
			res.Mode = "unknown"
			res.PolicyError = &authzStatusError{
				Code:    phelixerr.CodeOf(loadErr).String(),
				Message: phelixerr.Redact(loadErr.Error()),
			}
			res.Enforced = true
		default:
			res.PolicyLoaded = true
			res.Mode = policy.Mode
			res.Rules = len(policy.Rules)
			res.Enforced = policy.Mode == authz.ModeEnforced
		}

		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", res))
		}
		fmt.Printf("Mode:          %s\n", res.Mode)
		fmt.Printf("Policy:        %s\n", res.PolicyPath)
		if res.PolicyError != nil {
			fmt.Printf("Policy error:  %s: %s\n", res.PolicyError.Code, res.PolicyError.Message)
			fmt.Println("Protected execution is failing closed until this is fixed.")
		} else if !res.Enforced {
			fmt.Println("Authorization is not enforced: local CLI execution is permitted (legacy local mode).")
		} else {
			fmt.Printf("Rules:         %d\n", res.Rules)
		}
		fmt.Printf("Approvals:     %s\n", res.ApprovalsDir)
		fmt.Printf("Decision log:  %s\n", res.DecisionLogPath)
		fmt.Printf("Caller:        %s (authenticated=%t)\n", actor.Type, actor.Authenticated)
		return nil
	},
}

// --- authz check -------------------------------------------------------------

var authzCheckJSON bool

// authzCheckResult is the boundary's answer for one plan, computed WITHOUT
// executing anything and without recording an audit decision — see
// authz.Gate.Evaluate.
type authzCheckResult struct {
	PlanID     string         `json:"plan_id"`
	PlanHash   string         `json:"plan_hash"`
	Action     string         `json:"action"`
	Target     string         `json:"target"`
	Actor      *machine.Actor `json:"actor"`
	Decision   string         `json:"decision"`
	Code       string         `json:"code"`
	Reason     string         `json:"reason"`
	Mode       string         `json:"mode"`
	RuleIndex  int            `json:"rule_index"`
	ApprovalID string         `json:"approval_id,omitempty"`
	Approval   *authzApproval `json:"approval,omitempty"`
}

// authzApproval is the safe view of an approval artifact. approver_* states
// plainly what the approval proves: in this release an unauthenticated CLI
// caller with local-host filesystem authority, never a verified principal.
type authzApproval struct {
	ApprovalID            string `json:"approval_id"`
	PlanID                string `json:"plan_id"`
	PlanHash              string `json:"plan_hash"`
	Action                string `json:"action"`
	Target                string `json:"target"`
	Decision              string `json:"decision"`
	ApproverType          string `json:"approver_type"`
	ApproverID            string `json:"approver_id,omitempty"`
	ApproverAuthenticated bool   `json:"approver_authenticated"`
	ApproverSource        string `json:"approver_source"`
	ApprovedAtMs          int64  `json:"approved_at_ms"`
}

// approvalView projects an approval artifact into its safe machine view.
func approvalView(a *authz.Approval) *authzApproval {
	return &authzApproval{
		ApprovalID: a.ApprovalID, PlanID: a.PlanID, PlanHash: a.PlanHash,
		Action: a.Action, Target: a.Target, Decision: a.Decision,
		ApproverType: a.ApproverType, ApproverID: a.ApproverID,
		ApproverAuthenticated: a.ApproverAuthenticated,
		ApproverSource:        a.ApproverSource,
		ApprovedAtMs:          a.ApprovedAtMs,
	}
}

var authzCheckCmd = &cobra.Command{
	Use:   "check <plan-id>",
	Short: "Show what the authorization boundary would decide for a plan (no mutation)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if authzCheckJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		plan, err := plans.Load(args[0])
		if err != nil {
			return err
		}
		// Evaluate, not Authorize: `check` reports the decision instead of
		// failing on it, and runs the identical evaluation an apply would, so
		// the two can never disagree.
		out, err := authzGate.Evaluate(context.Background(), authz.ExecutionRequest{
			Action: plan.Action.Type,
			Target: authz.Target{App: plan.Action.Application, AppID: plan.Target.AppID},
			Plan:   &authz.PlanRef{ID: plan.PlanID, Hash: plan.PlanHash},
		})
		if err != nil {
			return err
		}
		res := &authzCheckResult{
			PlanID:     plan.PlanID,
			PlanHash:   plan.PlanHash,
			Action:     plan.Action.Type,
			Target:     plan.Action.Application,
			Actor:      out.Actor.Machine(),
			Decision:   out.Decision.Effect,
			Code:       out.Decision.Code,
			Reason:     phelixerr.Redact(out.Decision.Reason),
			Mode:       out.Decision.Mode,
			RuleIndex:  out.Decision.RuleIndex,
			ApprovalID: out.Decision.ApprovalID,
		}
		if a, loadErr := authz.LoadApproval(plan.PlanID); loadErr == nil && a != nil {
			res.Approval = approvalView(a)
		}

		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", res))
		}
		fmt.Printf("Plan:      %s\n", res.PlanID)
		fmt.Printf("Hash:      %s\n", res.PlanHash)
		fmt.Printf("Action:    %s on %s\n", res.Action, res.Target)
		fmt.Printf("Mode:      %s\n", res.Mode)
		fmt.Printf("Decision:  %s (%s)\n", res.Decision, res.Code)
		fmt.Printf("Reason:    %s\n", res.Reason)
		if res.Approval != nil {
			fmt.Printf("Approval:  %s for plan %s\n", res.Approval.ApprovalID, res.Approval.PlanID)
		}
		return nil
	},
}

// --- authz approve -----------------------------------------------------------

var (
	authzApproveJSON       bool
	authzApproveExpectHash string
)

var authzApproveCmd = &cobra.Command{
	Use:   "approve <plan-id>",
	Short: "Record an approval bound to one exact plan",
	Long: `Record an immutable approval for one exact plan.

The approval binds to the plan's id, content hash, action and target. It
authorizes nothing else: it cannot be reused for another plan, and it stops
matching if the plan's content hash ever differs from the one approved.

Pass --expect-hash to assert the plan hash you inspected; a mismatch is
refused rather than approved. Machine callers should always pass it, so an
approval is never granted to content the approver did not see.

This command is the only way Phelix creates an approval. Nothing approves
implicitly: no execution path, and no agent, can produce one as a side effect
of requesting execution.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if authzApproveJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		// The plan is loaded through the Phase 3 store, so its content hash is
		// re-verified before anything is approved: an approval can never be
		// granted against a corrupt or tampered plan.
		plan, err := plans.Load(args[0])
		if err != nil {
			return err
		}
		if authzApproveExpectHash != "" && authzApproveExpectHash != plan.PlanHash {
			return phelixerr.Newf(phelixerr.CodeApprovalInvalid,
				"plan %s has hash %s, but %s was expected — refusing to approve content you have not seen",
				plan.PlanID, plan.PlanHash, authzApproveExpectHash)
		}

		// The approver is the local host operator: what a local invocation
		// actually proves is write access to this host's Phelix data
		// directory. That is recorded honestly as an unauthenticated CLI
		// actor with source "local_host" — never dressed up as a verified
		// identity, and never derived from a caller-supplied name.
		approver, err := authz.LocalCLIAuthenticator{}.Authenticate()
		if err != nil {
			return err
		}
		approval, err := authz.NewApproval(approver,
			authz.PlanRef{ID: plan.PlanID, Hash: plan.PlanHash},
			plan.Action.Type, plan.Action.Application, authz.LocalProvenance())
		if err != nil {
			return err
		}
		if err := authz.SaveApproval(approval); err != nil {
			return err
		}

		view := approvalView(approval)
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", view))
		}
		fmt.Printf("Approved plan %s (%s on %s)\n", approval.PlanID, approval.Action, approval.Target)
		fmt.Printf("Approval:  %s\n", approval.ApprovalID)
		fmt.Printf("Bound to:  %s\n", approval.PlanHash)
		fmt.Printf("Approved:  %s\n", time.UnixMilli(approval.ApprovedAtMs).Format(time.RFC3339))
		return nil
	},
}

// --- authz revoke ------------------------------------------------------------

var authzRevokeJSON bool

var authzRevokeCmd = &cobra.Command{
	Use:   "revoke <plan-id>",
	Short: "Remove the approval recorded for a plan",
	Long: `Remove the approval recorded for a plan.

Approvals are immutable, so a changed decision is a revoke followed by a new
approve — never an edit. Revoking a plan that was already applied does not
undo the deployment; use 'phelix rollback' for that.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if authzRevokeJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		planID := args[0]
		removed, err := authz.RevokeApproval(planID)
		if err != nil {
			return err
		}
		if !removed {
			return phelixerr.Newf(phelixerr.CodeNotFound, "no approval recorded for plan %s", planID)
		}
		result := map[string]any{"plan_id": planID, "revoked": true}
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", result))
		}
		fmt.Printf("Revoked the approval for plan %s.\n", planID)
		return nil
	},
}

func init() {
	authzStatusCmd.Flags().BoolVar(&authzStatusJSON, "json", false, "Output the machine envelope on stdout")
	authzCheckCmd.Flags().BoolVar(&authzCheckJSON, "json", false, "Output the machine envelope on stdout")
	authzApproveCmd.Flags().BoolVar(&authzApproveJSON, "json", false, "Output the machine envelope on stdout")
	authzApproveCmd.Flags().StringVar(&authzApproveExpectHash, "expect-hash", "",
		"Approve only if the plan's content hash equals this value (sha256:…)")
	authzRevokeCmd.Flags().BoolVar(&authzRevokeJSON, "json", false, "Output the machine envelope on stdout")

	AuthzCmd.AddCommand(authzStatusCmd)
	AuthzCmd.AddCommand(authzCheckCmd)
	AuthzCmd.AddCommand(authzApproveCmd)
	AuthzCmd.AddCommand(authzRevokeCmd)
}
