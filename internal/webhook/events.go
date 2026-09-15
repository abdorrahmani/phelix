package webhook

import (
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/logs"
)

// Event actions emitted through the existing application-event channel. The
// webhook layer defines no event mechanism of its own: these are ordinary
// ApplicationEvents (action + success + redacted error message), delivered by
// the same reporter the CLI commands use, so the dashboard groups them with
// the app's other activity. Local logging with full identifiers
// (app, delivery id, branch, commit) happens at each emission site.
const (
	// EventActionServerStart: the webhook server came up (no app scope).
	EventActionServerStart = "webhook_server_start"
	// EventActionAccepted: a delivery was accepted and queued for rebuild.
	EventActionAccepted = "webhook_accepted"
	// EventActionRejected: an authenticated request was rejected after
	// signature verification (unparseable payload, missing delivery id).
	EventActionRejected = "webhook_rejected"
	// EventActionBranchMismatch: the push was authenticated but its branch
	// does not match the configured one (or the branch was deleted).
	EventActionBranchMismatch = "webhook_branch_mismatch"
	// EventActionDuplicate: a redelivered delivery ID; no second rebuild.
	EventActionDuplicate = "webhook_duplicate"
	// EventActionQueueFailure: an accepted delivery could not be queued, or a
	// queued job was dropped at shutdown.
	EventActionQueueFailure = "webhook_queue_failure"
	// EventActionRebuildFailed: the queued rebuild itself failed.
	EventActionRebuildFailed = "webhook_rebuild_failed"
)

// ReportEvent forwards one webhook event to the backend through the existing
// gRPC application-event reporter (fire-and-forget; offline or unwatched apps
// degrade to local logging only). Signature rejections are deliberately NOT
// reported this way — unauthenticated traffic must not be able to make the
// server emit authenticated backend traffic.
func ReportEvent(appID, appName, action string, success bool, errMsg string) {
	if errMsg != "" {
		logs.InfoFile("webhook", "event action=%s app=%s success=%v error=%q", action, appName, success, errMsg)
	} else {
		logs.InfoFile("webhook", "event action=%s app=%s success=%v", action, appName, success)
	}
	phelixgrpc.ReportEventAsync(appID, appName, action, success, errMsg, 0, "", "")
}
