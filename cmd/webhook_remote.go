package cmd

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/webhook"
)

// Remote webhook management: the MonitorStream command handlers that expose
// the existing webhook subsystem to the backend. There is deliberately no
// remote command that EXECUTES a webhook deployment — deployments originate
// only from the local HTTP webhook server (HMAC-authenticated provider
// deliveries); the remote surface is management and observability.
//
// Queries read the same durable state the local CLI reads (job records, the
// delivery ledger, phelix.yaml); mutations edit the same phelix.yaml webhook
// section through the project package's node-level saver, so local webhook
// execution (which loads phelix.yaml per delivery) observes the change
// immediately. The HMAC secret value is never read, returned or logged —
// only the environment-variable NAME crosses the wire.

// RemoteWebhookCommand executes one webhook_* MonitorStream command. It is
// registered with phelixgrpc.SetWebhookHandler by the monitor daemon; the
// gRPC dispatcher owns idempotency (queries bypass it, mutations are
// ledger-guarded) and stamps the identity fields.
func RemoteWebhookCommand(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	switch req.GetType() {
	case phelixgrpc.WebhookCommandStatus:
		return remoteWebhookStatus(req)
	case phelixgrpc.WebhookCommandHistory:
		return remoteWebhookHistory(req)
	case phelixgrpc.WebhookCommandConfig:
		return remoteWebhookConfig(req)
	case phelixgrpc.WebhookCommandDeliveries:
		return remoteWebhookDeliveries(req)
	case phelixgrpc.WebhookCommandEnable:
		return remoteWebhookMutate(req, 1, nil, nil)
	case phelixgrpc.WebhookCommandDisable:
		return remoteWebhookMutate(req, 0, nil, nil)
	case phelixgrpc.WebhookCommandSetBranch:
		branch := ""
		if opts := req.GetWebhook(); opts != nil {
			branch = opts.GetBranch()
		}
		return remoteWebhookMutate(req, -1, &branch, nil)
	case phelixgrpc.WebhookCommandSetSecretEnv:
		secretEnv := ""
		if opts := req.GetWebhook(); opts != nil {
			secretEnv = opts.GetSecretEnv()
		}
		return remoteWebhookMutate(req, -1, nil, &secretEnv)
	default:
		return remoteWebhookError(req, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"unknown webhook command type %q", req.GetType()))
	}
}

// remoteWebhookError builds a minimal error result; the dispatcher stamps
// RequestId/Command/AppName/Timestamp and redacts the message.
func remoteWebhookError(req *pb.MonitorCommandRequest, err error) *pb.MonitorCommandResult {
	return &pb.MonitorCommandResult{
		Status:    "error",
		Error:     err.Error(),
		ErrorCode: string(phelixerr.CodeOf(err)),
	}
}

// resolveWebhookApp resolves the request's app identity (stable ID
// preferred, name accepted — the same identifier vocabulary and ID-then-name
// lookup every other cmd-level command uses) to a managed application.
func resolveWebhookApp(identifier string) (*app.AppInfo, error) {
	return GetAppInfo(identifier)
}

// remoteWebhookStatus answers with the app's in-flight deployment jobs —
// the same records `phelix webhook status <app>` shows, newest first.
func remoteWebhookStatus(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	info, err := resolveWebhookApp(req.GetAppName())
	if err != nil {
		return remoteWebhookError(req, err)
	}
	store := webhook.OpenJobStoreReadOnly(webhookJobsDir())
	if err := store.Load(); err != nil {
		return remoteWebhookError(req, phelixerr.Wrap(phelixerr.CodeUnavailable, "webhook job store unavailable", err))
	}
	active := store.ActiveForApp(info.Name)
	result := &pb.MonitorCommandResult{Status: "success"}
	for _, rec := range active {
		result.WebhookJobs = append(result.WebhookJobs, toProtoWebhookJob(rec))
	}
	return result
}

// remoteWebhookHistory answers with the app's finished deployment jobs —
// the same records `phelix webhook history <app> --limit N` shows, newest
// first, bounded by the request's limit.
func remoteWebhookHistory(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	info, err := resolveWebhookApp(req.GetAppName())
	if err != nil {
		return remoteWebhookError(req, err)
	}
	limit := phelixgrpc.WebhookQueryLimit(0)
	if opts := req.GetWebhook(); opts != nil {
		limit = phelixgrpc.WebhookQueryLimit(opts.GetLimit())
	}
	store := webhook.OpenJobStoreReadOnly(webhookJobsDir())
	if err := store.Load(); err != nil {
		return remoteWebhookError(req, phelixerr.Wrap(phelixerr.CodeUnavailable, "webhook job store unavailable", err))
	}
	history := store.HistoryForApp(info.Name, limit)
	result := &pb.MonitorCommandResult{Status: "success"}
	for _, rec := range history {
		result.WebhookJobs = append(result.WebhookJobs, toProtoWebhookJob(rec))
	}
	return result
}

// remoteWebhookConfig answers with the app's webhook configuration. The
// secret_env_status reports whether the AGENT process sees the named
// variable set ("configured"/"missing"; "" when no secret_env is
// configured) — presence only, never the value; the webhook server process
// owns the authoritative startup check.
func remoteWebhookConfig(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	info, err := resolveWebhookApp(req.GetAppName())
	if err != nil {
		return remoteWebhookError(req, err)
	}
	state, err := webhookConfigState(info)
	if err != nil {
		return remoteWebhookError(req, err)
	}
	return &pb.MonitorCommandResult{Status: "success", WebhookConfig: state}
}

// remoteWebhookDeliveries answers with the app's accepted deliveries from
// the dedup ledger — metadata only (never bodies, signatures or headers).
func remoteWebhookDeliveries(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	info, err := resolveWebhookApp(req.GetAppName())
	if err != nil {
		return remoteWebhookError(req, err)
	}
	limit := phelixgrpc.WebhookQueryLimit(0)
	if opts := req.GetWebhook(); opts != nil {
		limit = phelixgrpc.WebhookQueryLimit(opts.GetLimit())
	}
	ledger := webhook.NewDeliveryLedger(deliveriesLedgerPath(), webhook.DefaultLedgerCapacity)
	if err := ledger.Initialize(); err != nil {
		return remoteWebhookError(req, phelixerr.Wrap(phelixerr.CodeUnavailable, "webhook delivery ledger unavailable", err))
	}
	result := &pb.MonitorCommandResult{Status: "success"}
	for _, d := range ledger.RecentForApp(info.Name, limit) {
		result.WebhookDeliveries = append(result.WebhookDeliveries, &pb.WebhookDelivery{
			DeliveryId: d.DeliveryID,
			AppName:    d.App,
			Branch:     d.Branch,
			Commit:     d.Commit,
			Provider:   d.Provider,
			ReceivedAt: d.ReceivedAt,
		})
	}
	return result
}

// remoteWebhookMutate applies one configuration mutation. enabled < 0 means
// "leave the flag as-is" (setters); branch/secretEnv pointers are nil when
// that field is not being changed. The mutation validates the RESULTING
// configuration with the same rules the local webhook server enforces at
// phelix.yaml load time, persists it through the project package's
// node-level saver (comments and unrelated keys survive), and returns the
// resulting state so the backend can reconcile without a follow-up query.
func remoteWebhookMutate(req *pb.MonitorCommandRequest, enabled int8, branch, secretEnv *string) *pb.MonitorCommandResult {
	info, err := resolveWebhookApp(req.GetAppName())
	if err != nil {
		return remoteWebhookError(req, err)
	}
	if info.Directory == "" {
		return remoteWebhookError(req, phelixerr.Newf(phelixerr.CodeConfiguration,
			"application %q has no project directory", info.Name))
	}

	// Current state: the same load the local webhook server performs per
	// delivery. A missing phelix.yaml is a valid starting point (empty
	// webhook section); a malformed one fails closed, never overwritten.
	var current *project.WebhookConfig
	cfg, err := project.Load(info.Directory)
	if err != nil {
		if phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
			return remoteWebhookError(req, phelixerr.Wrapf(phelixerr.CodeConfiguration, err,
				"application %q: invalid %s in %s", info.Name, project.FileName, info.Directory))
		}
		current = &project.WebhookConfig{}
	} else {
		current = cfg.Webhook
		if current == nil {
			current = &project.WebhookConfig{}
		}
	}

	next := &project.WebhookConfig{
		Enabled:   current.Enabled,
		Branch:    current.Branch,
		SecretEnv: current.SecretEnv,
	}
	if enabled >= 0 {
		next.Enabled = enabled == 1
	}
	if branch != nil {
		next.Branch = *branch
	}
	if secretEnv != nil {
		next.SecretEnv = *secretEnv
	}

	// The resulting configuration must satisfy the same validation as a
	// locally-edited phelix.yaml — e.g. enabling requires branch and
	// secret_env to be configured.
	if err := next.Validate(); err != nil {
		return remoteWebhookError(req, err)
	}
	if err := project.SaveWebhookConfig(info.Directory, next); err != nil {
		return remoteWebhookError(req, err)
	}

	state, err := webhookConfigState(info)
	if err != nil {
		return remoteWebhookError(req, err)
	}
	return &pb.MonitorCommandResult{Status: "success", WebhookConfig: state}
}

// webhookConfigState reads the app's webhook configuration and reports the
// secret environment variable's PRESENCE (never its value).
func webhookConfigState(info *app.AppInfo) (*pb.WebhookConfigState, error) {
	if info.Directory == "" {
		return nil, phelixerr.Newf(phelixerr.CodeConfiguration,
			"application %q has no project directory", info.Name)
	}
	state := &pb.WebhookConfigState{}
	cfg, err := project.Load(info.Directory)
	if err != nil {
		if phelixerr.CodeOf(err) == phelixerr.CodeNotFound {
			return state, nil // no phelix.yaml: webhook disabled
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err,
			"application %q: invalid %s in %s", info.Name, project.FileName, info.Directory)
	}
	if cfg.Webhook != nil {
		state.Enabled = cfg.Webhook.Enabled
		state.Branch = cfg.Webhook.Branch
		state.SecretEnv = cfg.Webhook.SecretEnv
	}
	switch {
	case state.SecretEnv == "":
		// no secret_env configured — nothing to report
	default:
		// Presence only — never the value. An empty value counts as missing,
		// the same judgment the local webhook startup validation makes.
		if v, ok := os.LookupEnv(state.SecretEnv); ok && strings.TrimSpace(v) != "" {
			state.SecretEnvStatus = "configured"
		} else {
			state.SecretEnvStatus = "missing"
		}
	}
	return state, nil
}

func toProtoWebhookJob(rec webhook.JobRecord) *pb.WebhookJob {
	return &pb.WebhookJob{
		JobId:        rec.ID,
		DeliveryId:   rec.DeliveryID,
		AppId:        rec.AppID,
		AppName:      rec.AppName,
		Branch:       rec.Branch,
		Commit:       rec.Commit,
		Provider:     rec.Provider,
		Status:       rec.Status,
		Stage:        rec.Stage,
		AcceptedAt:   rec.AcceptedAt,
		StartedAt:    rec.StartedAt,
		FinishedAt:   rec.FinishedAt,
		Version:      int32(rec.Version),
		ErrorCode:    rec.ErrorCode,
		ErrorMessage: rec.ErrorMessage,
	}
}

// deliveriesLedgerPath is the durable delivery ledger location (the same
// file the webhook server writes).
func deliveriesLedgerPath() string {
	return filepath.Join(webhookDataRoot(), webhook.DeliveryLedgerFile)
}
