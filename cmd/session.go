package cmd

import (
	"fmt"
	"os"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/session"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// SessionCmd is the machine-first agent-session tracking surface. A session
// groups ONE unit of agent work (inspect → plan → authorize → apply → verify)
// and lets an interrupted agent resume by reading durable state. It references
// existing plans/operations/deployments by id; it NEVER executes, authorizes,
// or copies their authoritative state. Session commands are not operations:
// they emit the read-shaped envelope (no operation_id), like `plan create`.
var SessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Create, inspect and track durable agent execution sessions (machine-first)",
	Long: "A session is a durable, auditable tracking record that groups one unit " +
		"of agent work and references the plans, operations and deployments it " +
		"touched. It is a tracking primitive — creating or modifying a session " +
		"grants no authorization and executes nothing. Use --json for the machine " +
		"envelope (docs/reference/machine-contract.md).",
}

// sessionActor resolves the provenance actor from the SAME authenticator the
// authorization boundary uses, so a session's recorded caller can never
// disagree with the authz actor and is never taken from client input: `cli`
// for the local CLI, `mcp` under `phelix mcp serve`. It is provenance only.
func sessionActor() *machine.Actor {
	if authzGate != nil && authzGate.Authenticator != nil {
		if a, err := authzGate.Authenticator.Authenticate(); err == nil {
			return a.Machine()
		}
	}
	return machine.CLIActor()
}

var (
	sessionCreateJSON    bool
	sessionCreateTitle   string
	sessionCreateApp     string
	sessionCreateProject string
)

var sessionCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new active session (creates no deployment, executes nothing)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if sessionCreateJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		s, err := session.Create(session.CreateOpts{
			Title:   sessionCreateTitle,
			Project: sessionCreateProject,
			App:     sessionCreateApp,
			Actor:   sessionActor(),
		})
		if err != nil {
			return err
		}
		// Bound the session store (best-effort). Only terminal sessions are
		// pruned, so the active session just created is never removed.
		pruneSessions()
		return emitSession(s, "created")
	},
}

var (
	sessionShowJSON      bool
	sessionShowNoResolve bool
)

var sessionShowCmd = &cobra.Command{
	Use:   "show <session-id>",
	Short: "Show a session, its references (resolved live) and bounded event history",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if sessionShowJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		res, err := session.Show(strings.TrimSpace(args[0]), !sessionShowNoResolve)
		if err != nil {
			return err
		}
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", res))
		}
		printSessionHuman(res)
		return nil
	},
}

var (
	sessionListJSON   bool
	sessionListStatus string
	sessionListApp    string
	sessionListLimit  int
)

var sessionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List sessions (newest first), optionally filtered by status and app",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if sessionListJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		if sessionListLimit < 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "--limit must be >= 0 (0 = all)")
		}
		if sessionListStatus != "" && !session.ValidStatus(sessionListStatus) {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"invalid --status %q: expected active, completed, failed or cancelled", sessionListStatus)
		}
		list, skipped, truncated, err := session.List(sessionListStatus, sessionListApp, sessionListLimit)
		if err != nil {
			return err
		}
		if skipped > 0 {
			logs.WarningFile("session", "%d corrupt session record(s) skipped", skipped)
		}
		res := session.ListResult{Sessions: list, Count: len(list), Truncated: truncated}
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", res))
		}
		printSessionListHuman(res)
		return nil
	},
}

var (
	sessionCheckpointJSON    bool
	sessionCheckpointNote    string
	sessionCheckpointStep    string
	sessionCheckpointPlans   []string
	sessionCheckpointOps     []string
	sessionCheckpointDeploys []string
	sessionCheckpointIfRev   int
)

var sessionCheckpointCmd = &cobra.Command{
	Use:   "checkpoint <session-id>",
	Short: "Record a bounded workflow checkpoint and/or attach plan/operation/deployment references",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if sessionCheckpointJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		s, err := session.Checkpoint(strings.TrimSpace(args[0]), session.CheckpointOpts{
			Step:      sessionCheckpointStep,
			Note:      sessionCheckpointNote,
			Refs:      refsFromFlags(sessionCheckpointPlans, sessionCheckpointOps, sessionCheckpointDeploys),
			ExpectRev: sessionCheckpointIfRev,
		})
		if err != nil {
			return err
		}
		return emitSession(s, "checkpointed")
	},
}

var (
	sessionCompleteJSON    bool
	sessionCompleteResult  string
	sessionCompletePlans   []string
	sessionCompleteOps     []string
	sessionCompleteDeploys []string
	sessionCompleteIfRev   int
)

var sessionCompleteCmd = &cobra.Command{
	Use:   "complete <session-id>",
	Short: "Mark a session completed (tracking only — does not verify or change any deployment)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if sessionCompleteJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		s, err := session.Complete(strings.TrimSpace(args[0]), session.CompleteOpts{
			Result:    sessionCompleteResult,
			Refs:      refsFromFlags(sessionCompletePlans, sessionCompleteOps, sessionCompleteDeploys),
			ExpectRev: sessionCompleteIfRev,
		})
		if err != nil {
			return err
		}
		return emitSession(s, "completed")
	},
}

var (
	sessionFailJSON    bool
	sessionFailReason  string
	sessionFailPlans   []string
	sessionFailOps     []string
	sessionFailDeploys []string
	sessionFailIfRev   int
)

var sessionFailCmd = &cobra.Command{
	Use:   "fail <session-id>",
	Short: "Mark a session failed (records a reported outcome; stops/redeploys nothing)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if sessionFailJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		s, err := session.Fail(strings.TrimSpace(args[0]), session.FailOpts{
			Reason:    sessionFailReason,
			Refs:      refsFromFlags(sessionFailPlans, sessionFailOps, sessionFailDeploys),
			ExpectRev: sessionFailIfRev,
		})
		if err != nil {
			return err
		}
		return emitSession(s, "failed")
	},
}

var (
	sessionCancelJSON   bool
	sessionCancelReason string
	sessionCancelIfRev  int
)

var sessionCancelCmd = &cobra.Command{
	Use:   "cancel <session-id>",
	Short: "Cancel a session's tracking workflow (does NOT cancel any running operation or deployment)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if sessionCancelJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		s, err := session.Cancel(strings.TrimSpace(args[0]), session.CancelOpts{
			Reason:    sessionCancelReason,
			ExpectRev: sessionCancelIfRev,
		})
		if err != nil {
			return err
		}
		return emitSession(s, "cancelled")
	},
}

func refsFromFlags(plans, operations, deployments []string) session.Refs {
	return session.Refs{Plans: plans, Operations: operations, Deployments: deployments}
}

// emitSession writes the machine envelope (when active) or a short human
// confirmation for a session returned by a mutating command. A session is its
// own ses_ artifact, not an operation, so the envelope carries no
// operation_id (empty first arg to machine.Success), exactly like plan create.
func emitSession(s *session.Session, verb string) error {
	if machine.Active() {
		return writeEnvelopeResult(machine.Success("", &session.SessionResult{Session: s}))
	}
	fmt.Printf("  %s session %s %s\n", color.GreenString("✓"), s.SessionID, verb)
	fmt.Printf("    status: %s   rev: %d\n", s.Status, s.Rev)
	if n := len(s.PlanIDs) + len(s.OperationIDs) + len(s.DeploymentIDs); n > 0 {
		fmt.Printf("    refs:   %d plan(s), %d operation(s), %d deployment(s)\n",
			len(s.PlanIDs), len(s.OperationIDs), len(s.DeploymentIDs))
	}
	return nil
}

func printSessionHuman(res *session.SessionResult) {
	s := res.Session
	fmt.Printf("Session %s  [%s]\n", s.SessionID, s.Status)
	if s.Title != "" {
		fmt.Printf("  title:   %s\n", s.Title)
	}
	if s.App != "" {
		fmt.Printf("  app:     %s\n", s.App)
	}
	if s.Project != "" {
		fmt.Printf("  project: %s\n", s.Project)
	}
	fmt.Printf("  rev:     %d\n", s.Rev)
	if s.Checkpoint != nil {
		fmt.Printf("  checkpoint: %s\n", strings.TrimSpace(s.Checkpoint.Step+" "+s.Checkpoint.Note))
	}
	if s.FinalResult != "" {
		fmt.Printf("  result:  %s\n", s.FinalResult)
	}
	if res.Resolved != nil {
		printResolvedRefs("plans", res.Resolved.Plans)
		printResolvedRefs("operations", res.Resolved.Operations)
		printResolvedRefs("deployments", res.Resolved.Deployments)
	} else {
		printIDs("plans", s.PlanIDs)
		printIDs("operations", s.OperationIDs)
		printIDs("deployments", s.DeploymentIDs)
	}
	if len(s.Events) > 0 {
		fmt.Println("  events:")
		for _, e := range s.Events {
			fmt.Printf("    #%d %-10s %s\n", e.Seq, e.Type, e.Detail)
		}
	}
}

func printIDs(kind string, ids []string) {
	if len(ids) == 0 {
		return
	}
	fmt.Printf("  %s: %s\n", kind, strings.Join(ids, ", "))
}

func printResolvedRefs(kind string, refs []session.RefView) {
	if len(refs) == 0 {
		return
	}
	fmt.Printf("  %s:\n", kind)
	for _, r := range refs {
		line := fmt.Sprintf("    %s  %s", r.ID, r.State)
		if r.Status != "" {
			line += " (" + r.Status + ")"
		}
		if r.ErrorCode != "" {
			line += " [" + r.ErrorCode + "]"
		}
		fmt.Println(line)
	}
}

func printSessionListHuman(res session.ListResult) {
	if len(res.Sessions) == 0 {
		fmt.Println("No sessions recorded. Create one with 'phelix session create'.")
		return
	}
	w := os.Stdout
	fmt.Fprintln(w, "ID\tSTATUS\tAPP\tREV\tTITLE\tAGE")
	for _, s := range res.Sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
			s.SessionID, s.Status, s.App, s.Rev, truncateTitle(s.Title), relTime(timeFromMillis(s.CreatedAt)))
	}
	if res.Truncated {
		fmt.Fprintln(w, "… more sessions exist; raise --limit to see them")
	}
}

func truncateTitle(t string) string {
	const max = 40
	if len(t) > max {
		return t[:max-1] + "…"
	}
	return t
}

func init() {
	sessionCreateCmd.Flags().BoolVar(&sessionCreateJSON, "json", false, "Output the machine envelope on stdout")
	sessionCreateCmd.Flags().StringVar(&sessionCreateTitle, "title", "", "Short task description (bounded, redacted)")
	sessionCreateCmd.Flags().StringVar(&sessionCreateApp, "app", "", "Application this session concerns")
	sessionCreateCmd.Flags().StringVar(&sessionCreateProject, "project", "", "Project this session concerns")

	sessionShowCmd.Flags().BoolVar(&sessionShowJSON, "json", false, "Output the machine envelope on stdout")
	sessionShowCmd.Flags().BoolVar(&sessionShowNoResolve, "no-resolve", false, "Do not resolve referenced plans/operations/deployments")

	sessionListCmd.Flags().BoolVar(&sessionListJSON, "json", false, "Output the machine envelope on stdout")
	sessionListCmd.Flags().StringVar(&sessionListStatus, "status", "", "Filter by status: active, completed, failed or cancelled")
	sessionListCmd.Flags().StringVar(&sessionListApp, "app", "", "Filter by application")
	sessionListCmd.Flags().IntVar(&sessionListLimit, "limit", 0, "Maximum sessions to list (0 = all)")
}

func addRefFlags(c *cobra.Command, plans, ops, deploys *[]string, ifRev *int) {
	c.Flags().StringArrayVar(plans, "plan", nil, "Plan id to link (repeatable)")
	c.Flags().StringArrayVar(ops, "operation", nil, "Operation id to link (repeatable)")
	c.Flags().StringArrayVar(deploys, "deployment", nil, "Deployment id to link (repeatable)")
	c.Flags().IntVar(ifRev, "if-rev", -1, "Only apply if the session is at this revision (optimistic concurrency)")
}

func init() {
	sessionCheckpointCmd.Flags().BoolVar(&sessionCheckpointJSON, "json", false, "Output the machine envelope on stdout")
	sessionCheckpointCmd.Flags().StringVar(&sessionCheckpointNote, "note", "", "Bounded free-text note (redacted)")
	sessionCheckpointCmd.Flags().StringVar(&sessionCheckpointStep, "step", "", "Short workflow step label")
	addRefFlags(sessionCheckpointCmd, &sessionCheckpointPlans, &sessionCheckpointOps, &sessionCheckpointDeploys, &sessionCheckpointIfRev)

	sessionCompleteCmd.Flags().BoolVar(&sessionCompleteJSON, "json", false, "Output the machine envelope on stdout")
	sessionCompleteCmd.Flags().StringVar(&sessionCompleteResult, "result", "", "Bounded completion summary (reported, not verified)")
	addRefFlags(sessionCompleteCmd, &sessionCompletePlans, &sessionCompleteOps, &sessionCompleteDeploys, &sessionCompleteIfRev)

	sessionFailCmd.Flags().BoolVar(&sessionFailJSON, "json", false, "Output the machine envelope on stdout")
	sessionFailCmd.Flags().StringVar(&sessionFailReason, "reason", "", "Bounded failure summary (reported, not verified)")
	addRefFlags(sessionFailCmd, &sessionFailPlans, &sessionFailOps, &sessionFailDeploys, &sessionFailIfRev)

	sessionCancelCmd.Flags().BoolVar(&sessionCancelJSON, "json", false, "Output the machine envelope on stdout")
	sessionCancelCmd.Flags().StringVar(&sessionCancelReason, "reason", "", "Bounded reason for cancelling tracking")
	sessionCancelCmd.Flags().IntVar(&sessionCancelIfRev, "if-rev", -1, "Only apply if the session is at this revision (optimistic concurrency)")

	SessionCmd.AddCommand(sessionCreateCmd, sessionShowCmd, sessionListCmd,
		sessionCheckpointCmd, sessionCompleteCmd, sessionFailCmd, sessionCancelCmd)
}
