package grpc

import (
	"fmt"
	"sort"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

// Read-only rollback query command dispatch. The frontend drives the two
// read-only rollback surfaces — the version list (`rollback --list`) and the
// rollback history (`rollback history`) — over the MonitorStream, exactly like
// matrix_status/matrix_list and the webhook queries:
//
//   - they are READ-ONLY: no idempotency ledger entry (a query has no
//     execution to make idempotent and the backend may re-ask at will), and no
//     execution authorization gate (the local --list / history paths are
//     ungated too);
//   - they reuse the SAME emission the local CLI and the automatic version
//     sync use: the answer travels as a RollbackLifecycleEvent (list_versions
//     or history step) over ReportRollbackEvent, correlated to the command by
//     request_id. The MonitorCommandResult is only a synchronous
//     acknowledgement (success, or a validation/resolution error) — there is
//     deliberately no second copy of the data on the result message.
//
// Because the data rides the best-effort event stream (see
// enqueueRollbackEvent), a query is re-askable: if the event does not arrive,
// the backend simply re-issues the command. The command types are new string
// values only — the request shape is unchanged except for the shared `limit`.

// Rollback query command types (MonitorCommandRequest.type).
const (
	RollbackCommandList    = "rollback_list"
	RollbackCommandHistory = "rollback_history"
)

// Query answer bounds. list defaults to "every retained version" (retention is
// small); history defaults to 20, matching the local `rollback history` flag.
// Both are hard-capped so a hostile or buggy limit can never produce an
// oversized event.
// ponytail: fixed 100-row cap. Raise it if a UI ever needs a deeper
// single-shot page; the agent keeps no cursor, so paging past the cap is not
// supported today.
const (
	rollbackQueryHistoryDefault = 20
	rollbackQueryLimitMax       = 100
)

// isRollbackQueryCommand reports whether a command type is a read-only rollback
// query (routed away from the ledger-guarded execute path).
func isRollbackQueryCommand(t string) bool {
	return t == RollbackCommandList || t == RollbackCommandHistory
}

// validateRollbackQueryCommand checks the transport-level shape before any
// read: correlation is required (request_id is how the backend ties the
// emitted list/history event back to this query), the app must be named, the
// limit must be non-negative, and no foreign option fields may ride along.
func validateRollbackQueryCommand(req *pb.MonitorCommandRequest) error {
	if req.GetRequestId() == "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires a request_id", req.GetType())
	}
	if req.GetAppName() == "" {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "%s command requires an app_name", req.GetType())
	}
	if req.GetStrategy() != "" || req.GetReplicas() != 0 || req.GetCanaryPercent() != 0 ||
		req.GetReason() != "" || req.GetVerifyDurationMs() != 0 || req.GetDryRun() ||
		req.GetTarget() != "" || req.GetMatrix() != nil || req.GetWebhook() != nil {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s is a read-only query and accepts only app_name and limit", req.GetType())
	}
	if req.GetLimit() < 0 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "limit must be 0 or positive, got %d", req.GetLimit())
	}
	return nil
}

// handleRollbackQueryCommand answers one read-only rollback query
// synchronously: validate, resolve the app, emit the list/history event (the
// data channel), and acknowledge via the command result. No ledger, no gate,
// no metrics pause — a query is cheap and side-effect-free.
func (c *Client) handleRollbackQueryCommand(req *pb.MonitorCommandRequest) {
	if err := validateRollbackQueryCommand(req); err != nil {
		_ = sendCommandResultLogged(finalizeRollbackQueryResult(req, commandErrorResult(req, err)))
		return
	}
	_ = sendCommandResultLogged(executeRollbackQuery(req))
}

// executeRollbackQuery resolves the app and emits the answer event, returning
// the acknowledgement result (success, or the resolution/read error).
func executeRollbackQuery(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	appID, appName, err := resolveRollbackQueryApp(req.GetAppName())
	if err != nil {
		return finalizeRollbackQueryResult(req, commandErrorResult(req, err))
	}
	switch req.GetType() {
	case RollbackCommandList:
		err = emitRollbackVersionListQuery(appID, appName, int(req.GetLimit()), req.GetRequestId())
	case RollbackCommandHistory:
		err = emitRollbackHistoryQuery(appID, appName, int(req.GetLimit()), req.GetRequestId())
	default:
		err = phelixerr.Newf(phelixerr.CodeInvalidArgument, "unknown rollback query type %q", req.GetType())
	}
	if err != nil {
		return finalizeRollbackQueryResult(req, commandErrorResult(req, err))
	}
	return finalizeRollbackQueryResult(req, &pb.MonitorCommandResult{Status: "success"})
}

// resolveRollbackQueryApp resolves a managed app by name or ID through the
// daemon's in-memory manager — the same lookup every remote command uses.
func resolveRollbackQueryApp(identifier string) (id, name string, err error) {
	for _, a := range app.Manager.ListApplications() {
		if a.Name == identifier || a.ID == identifier {
			return a.ID, a.Name, nil
		}
	}
	return "", "", phelixerr.Newf(phelixerr.CodeNotFound, "app not found: %s", identifier)
}

// emitRollbackVersionListQuery emits the version list as a
// RollbackLifecycleEvent correlated by request_id, reusing the shared mapping.
// limit <= 0 lists every retained version; a positive limit truncates. The
// read is local-log-free — a query must not mutate audit state.
func emitRollbackVersionListQuery(appID, appName string, limit int, requestID string) error {
	policy := deploy.DefaultRetention{Max: 5}
	vers, err := deploy.ListVersionsForDisplay(appName, policy)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "list versions", err)
	}
	if limit > rollbackQueryLimitMax {
		limit = rollbackQueryLimitMax
	}
	if limit > 0 && len(vers) > limit {
		vers = vers[:limit]
	}
	r := NewRollbackReporter("", appID, appID, appName, "list", "list", "", "", "")
	r.DisableLocalLog()
	r.SetRequestID(requestID)
	r.SetVersionList(rollbackVersionEntries(appName, vers, policy))
	r.SetMetadata("sync_mode", "query")
	r.SetMetadata("version_count", fmt.Sprintf("%d", len(vers)))
	r.Emit(RollbackStepListVersions, true, fmt.Sprintf("listed %d versions", len(vers)), 0, "")
	return nil
}

// emitRollbackHistoryQuery emits the rollback history, newest first, bounded by
// limit (<= 0 → default 20, hard cap 100), as a history-step event correlated
// by request_id. Mirrors the local `rollback history` emission.
func emitRollbackHistoryQuery(appID, appName string, limit int, requestID string) error {
	records, skipped, err := deploy.ReadRollbackHistory(appName)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "read rollback history", err)
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].Time.After(records[j].Time) })
	if limit <= 0 {
		limit = rollbackQueryHistoryDefault
	}
	if limit > rollbackQueryLimitMax {
		limit = rollbackQueryLimitMax
	}
	if len(records) > limit {
		records = records[:limit]
	}
	r := NewRollbackReporter("", appID, appID, appName, "history", "history", "", "", "")
	r.DisableLocalLog()
	r.SetRequestID(requestID)
	r.SetHistory(rollbackHistoryEntriesFromRecords(records))
	if skipped > 0 {
		r.SetMetadata("malformed_skipped", fmt.Sprintf("%d", skipped))
	}
	r.SetMetadata("limit", fmt.Sprintf("%d", limit))
	r.SetMetadata("record_count", fmt.Sprintf("%d", len(records)))
	r.Emit(RollbackStepHistory, true, fmt.Sprintf("reported %d rollback history records", len(records)), 0, "")
	return nil
}

// rollbackHistoryEntriesFromRecords maps structured history records onto the
// wire message.
// ponytail: a small copy of cmd.rollbackHistoryEntries — the cmd package
// cannot be imported here (it imports this one), and mirroring an 11-field map
// is cheaper than hoisting it into a third shared package.
func rollbackHistoryEntriesFromRecords(records []deploy.RollbackHistoryRecord) []*pb.RollbackHistoryEntry {
	entries := make([]*pb.RollbackHistoryEntry, 0, len(records))
	for _, rec := range records {
		e := &pb.RollbackHistoryEntry{
			Time:        rec.Time.UnixMilli(),
			FromVersion: rec.From,
			ToVersion:   rec.To,
			Status:      rec.Status,
			Mode:        rec.Mode,
			Reason:      rec.Reason,
			Source:      rec.Source,
			Error:       rec.Error,
		}
		if rec.Verification != nil {
			e.VerifyStatus = rec.Verification.Status
			e.VerifyDuration = rec.Verification.Duration
			e.VerifyError = rec.Verification.Error
		}
		entries = append(entries, e)
	}
	return entries
}

// finalizeRollbackQueryResult stamps the identity fields and redacts the error
// text, exactly like finalizeMatrixResult / finalizeWebhookResult.
func finalizeRollbackQueryResult(req *pb.MonitorCommandRequest, result *pb.MonitorCommandResult) *pb.MonitorCommandResult {
	result.RequestId = req.GetRequestId()
	result.Command = req.GetType()
	result.AppName = req.GetAppName()
	result.Timestamp = time.Now().UnixMilli()
	if result.Status == "error" {
		if result.ErrorCode == "" {
			result.ErrorCode = string(phelixerr.CodeUnknown)
		}
		result.Error = phelixerr.Redact(result.Error)
	}
	return result
}
