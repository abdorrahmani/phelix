package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/matrix"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/abdorrahmani/phelix/internal/webhook"
	"github.com/spf13/cobra"
)

// OperationCmd is the machine-facing operation query path: an operation ID
// (op_, wh_, mx_ or dep-) resolves to the operation's current external state,
// result and error. It reads only the durable records — it never re-derives
// state from live processes.
var OperationCmd = &cobra.Command{
	Use:   "operation",
	Short: "Inspect mutation operations by their durable IDs",
}

var operationStatusJSON bool

var operationStatusCmd = &cobra.Command{
	Use:   "status <operation-id>",
	Short: "Show the current state, result and error of one operation",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if operationStatusJSON {
			restore := machine.EnterJSON()
			defer restore()
		}

		view, err := resolveOperationView(strings.TrimSpace(args[0]))
		if err != nil {
			return err
		}
		return writeEnvelopeResult(machine.Success(view.OperationID, view))
	},
}

var operationListJSON bool
var operationListApp string
var operationListLimit int

var operationListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recorded mutation operations (newest first)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if operationListJSON {
			restore := machine.EnterJSON()
			defer restore()
		}

		if operationListApp != "" {
			if err := app.Manager.LoadState(); err != nil {
				return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
			}
			info, err := GetAppInfo(operationListApp)
			if err != nil {
				return err
			}
			operationListApp = info.Name
		}

		recs, skipped, err := ops.List(operationListApp, operationListLimit)
		if err != nil {
			return err
		}
		views := make([]operationView, 0, len(recs))
		for _, rec := range recs {
			views = append(views, operationViewFromRecord(rec))
		}
		table, err := writeOperationListOutput(views, skipped)
		if err != nil {
			return err
		}
		_ = table
		return nil
	},
}

// operationView is the machine-contract view of one operation. Status uses
// the external lifecycle vocabulary; internal_status preserves the owning
// subsystem's own state so mapping never loses information.
type operationView struct {
	OperationID    string          `json:"operation_id"`
	Kind           string          `json:"kind"`
	App            string          `json:"app,omitempty"`
	Status         string          `json:"status"`
	InternalStatus string          `json:"internal_status,omitempty"`
	Source         string          `json:"source"` // "operation_record" | "webhook_job" | "matrix_run" | "deployment_state"
	DeploymentID   string          `json:"deployment_id,omitempty"`
	PlanID         string          `json:"plan_id,omitempty"`
	PlanHash       string          `json:"plan_hash,omitempty"`
	AuthzDecision  string          `json:"authz_decision_id,omitempty"`
	ApprovalID     string          `json:"approval_id,omitempty"`
	RequestKey     string          `json:"request_key,omitempty"`
	Actor          *machine.Actor  `json:"actor,omitempty"`
	PID            int             `json:"pid,omitempty"`
	CreatedAt      int64           `json:"created_at_ms,omitempty"`
	UpdatedAt      int64           `json:"updated_at_ms,omitempty"`
	FinishedAt     int64           `json:"finished_at_ms,omitempty"`
	Result         *ops.Result     `json:"result,omitempty"`
	Error          *operationError `json:"error,omitempty"`
}

type operationError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func operationViewFromRecord(rec *ops.Record) operationView {
	v := operationView{
		OperationID:    rec.ID,
		Kind:           rec.Kind,
		App:            rec.App,
		Status:         rec.Status,
		InternalStatus: rec.Status,
		Source:         "operation_record",
		DeploymentID:   rec.DeploymentID,
		PlanID:         rec.PlanID,
		PlanHash:       rec.PlanHash,
		AuthzDecision:  rec.AuthzDecisionID,
		ApprovalID:     rec.ApprovalID,
		RequestKey:     rec.RequestKey,
		Actor:          rec.Actor,
		PID:            rec.PID,
		CreatedAt:      rec.CreatedAt,
		UpdatedAt:      rec.UpdatedAt,
		FinishedAt:     rec.FinishedAt,
		Result:         rec.Result,
	}
	if rec.Error != nil {
		v.Error = &operationError{Code: rec.Error.Code, Message: rec.Error.Message}
	}
	return v
}

// resolveOperationView resolves any operation ID vocabulary to its current
// state. The ID prefix decides the store: op_ records are the native store;
// wh_ resolves through the webhook job store; mx_ through the matrix run
// history; dep- correlates against operation records and, failing that, the
// app's last recorded deployment.
func resolveOperationView(id string) (*operationView, error) {
	switch {
	case strings.HasPrefix(id, ops.IDPrefix):
		rec, err := ops.Load(id)
		if err != nil {
			return nil, err
		}
		view := operationViewFromRecord(rec)
		return &view, nil

	case strings.HasPrefix(id, "wh_"):
		store := webhook.OpenJobStoreReadOnly(webhookJobsDir())
		rec, ok := store.Get(id)
		if !ok {
			return nil, phelixerr.Newf(phelixerr.CodeNotFound, "no webhook job %s", id)
		}
		view := &operationView{
			OperationID:    rec.ID,
			Kind:           "webhook_deploy",
			App:            rec.AppName,
			Status:         machine.MapWebhookJobStatus(rec.Status),
			InternalStatus: rec.Status,
			Source:         "webhook_job",
			CreatedAt:      rec.AcceptedAt,
			FinishedAt:     rec.FinishedAt,
			Error:          nil,
		}
		if rec.ErrorCode != "" || rec.ErrorMessage != "" {
			view.Error = &operationError{Code: rec.ErrorCode, Message: rec.ErrorMessage}
		}
		if rec.Version > 0 {
			view.Result = &ops.Result{Version: rec.Version}
		}
		return view, nil

	case strings.HasPrefix(id, "mx_"):
		run, err := matrix.LoadRun(matrix.RunID(id))
		if err != nil {
			return nil, phelixerr.Wrap(phelixerr.CodeNotFound, "no matrix run with that id", err)
		}
		view := &operationView{
			OperationID:    string(run.ID),
			Kind:           "matrix_build",
			App:            run.AppName,
			Status:         machine.MapMatrixRunStatus(string(run.Status)),
			InternalStatus: string(run.Status),
			Source:         "matrix_run",
			CreatedAt:      run.StartedAt.UnixMilli(),
		}
		if !run.FinishedAt.IsZero() {
			view.FinishedAt = run.FinishedAt.UnixMilli()
		}
		if run.Status == matrix.RunStatusSucceeded && run.Succeeded > 0 {
			view.Result = &ops.Result{Strategy: "matrix"}
		}
		return view, nil

	case strings.HasPrefix(id, "dep-"):
		rec, recErr := ops.FindByDeploymentID(id)
		if recErr == nil {
			view := operationViewFromRecord(rec)
			return &view, nil
		}
		// Fall back to the app's last recorded deployment (deploy.json keeps
		// only the most recent dep- id; older ones are resolved through the
		// operation records above).
		if err := app.Manager.LoadState(); err != nil {
			return nil, recErr
		}
		for _, a := range app.Manager.ListApplications() {
			st, err := deploy.Load(a.Name)
			if err != nil || st == nil || st.LastDeploymentID != id {
				continue
			}
			return &operationView{
				OperationID:    id,
				Kind:           "deploy",
				App:            a.Name,
				Status:         machine.StatusSucceeded,
				InternalStatus: "succeeded",
				Source:         "deployment_state",
				DeploymentID:   id,
				Result:         &ops.Result{Version: st.ActiveVersion},
			}, nil
		}
		return nil, recErr

	default:
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"unrecognized operation id %q: expected op_…, wh_…, mx_… or dep-…", id)
	}
}

func init() {
	operationStatusCmd.Flags().BoolVar(&operationStatusJSON, "json", false,
		"Output machine-readable JSON (stdout carries only the result envelope; human output moves to stderr)")
	operationListCmd.Flags().BoolVar(&operationListJSON, "json", false,
		"Output machine-readable JSON (stdout carries only the result envelope; table moves to stderr)")
	operationListCmd.Flags().StringVar(&operationListApp, "app", "", "Filter operations by application")
	operationListCmd.Flags().IntVar(&operationListLimit, "limit", 20, "Maximum operations to list (0 = all)")

	OperationCmd.AddCommand(operationStatusCmd)
	OperationCmd.AddCommand(operationListCmd)
}

// writeOperationListOutput renders the list as a human table (stderr in
// machine mode) and, in machine mode, the JSON envelope on stdout.
func writeOperationListOutput(views []operationView, skipped int) (bool, error) {
	if skipped > 0 {
		logs.WarningFile("ops", "%d corrupt operation record(s) skipped", skipped)
	}
	if machine.Active() {
		return true, writeEnvelopeResult(machine.Success("", operationListResult{
			Operations: views,
			Count:      len(views),
		}))
	}
	_ = skipped
	if len(views) == 0 {
		fmt.Println("No operations recorded yet. Mutation commands (rebuild, rollback, build) record their operations here.")
		return false, nil
	}
	w := os.Stdout
	fmt.Fprintln(w, "ID\tKIND\tAPP\tSTATUS\tVERSION\tDEPLOYMENT\tAGE")
	for _, v := range views {
		version := ""
		if v.Result != nil && v.Result.Version > 0 {
			version = fmt.Sprintf("v%d", v.Result.Version)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			v.OperationID, v.Kind, v.App, v.Status, version, v.DeploymentID, relTime(timeFromMillis(v.CreatedAt)))
	}
	return false, nil
}

type operationListResult struct {
	Operations []operationView `json:"operations"`
	Count      int             `json:"count"`
}

// timeFromMillis converts ledger-style unix-millis to a time for the human
// table.
func timeFromMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
