package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/webhook"
	"github.com/spf13/cobra"
)

var (
	webhookHost string
	webhookPort int
)

const (
	// webhookHTTPDrain bounds graceful HTTP shutdown (handlers are fast —
	// they only enqueue).
	webhookHTTPDrain = 10 * time.Second
	// webhookQueueDrain bounds how long shutdown waits for the rebuild job
	// in flight. A rebuild subprocess still running past it is left to
	// finish on its own (it is an independent CLI invocation that releases
	// the deploy lock and records versions itself).
	webhookQueueDrain = 60 * time.Second
)

// WebhookCmd starts the Git-push webhook server. It is a long-running daemon
// like `phelix monitor`: runs in the foreground, exits cleanly on
// SIGINT/SIGTERM.
var WebhookCmd = &cobra.Command{
	Use:   "webhook",
	Short: "Start the Git push webhook server",
	Long: `Start the Git push webhook server.

The server accepts GitHub-style push webhooks:

  POST /webhook/<app>

authenticated by an HMAC-SHA256 signature (X-Hub-Signature-256) over the raw
request body, using the shared secret named by webhook.secret_env in the
app's phelix.yaml. A push to the configured webhook.branch hands the app to
the build queue, which triggers the SAME rebuild pipeline as
'phelix rebuild' (strategy, health checks, versioning, rollback and the
deploy lock all stay inside it). The HTTP response returns as soon as the
job is queued — it never waits for the rebuild.

The secret itself is never stored in phelix.yaml and never logged. Apps
without a webhook section (or with enabled: false) answer 404, exactly like
unknown apps.

Runs in the foreground; exit with Ctrl-C or SIGTERM.`,
	RunE: func(cobraCmd *cobra.Command, args []string) error {
		return runWebhook()
	},
}

func init() {
	WebhookCmd.Flags().StringVar(&webhookHost, "host", "127.0.0.1", "Address to listen on (use 0.0.0.0 to accept webhooks from the network)")
	WebhookCmd.Flags().IntVar(&webhookPort, "port", 9746, "Port to listen on")
}

// runWebhook is the daemon entry point. It fails fast on unrecoverable
// startup errors (unreadable app state, enabled webhooks without their
// secret, a corrupt delivery ledger, a busy port) and otherwise blocks until
// a shutdown signal.
func runWebhook() error {
	if err := app.Manager.LoadState(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
	}

	// An enabled webhook without its secret must fail clearly at startup —
	// otherwise every delivery for that app would fail at request time.
	if err := webhook.ValidateStartupSecrets(os.LookupEnv); err != nil {
		return err
	}

	// The delivery ledger is the durable replay protection; a corrupt one
	// fails startup closed instead of accepting undededuplicated deliveries.
	ledger := webhook.NewDeliveryLedger(
		filepath.Join(server.DataDir(), "webhook", webhook.DeliveryLedgerFile),
		webhook.DefaultLedgerCapacity,
	)
	if err := ledger.Initialize(); err != nil {
		return err
	}

	// Agent identity for event attribution.
	if err := server.Initialize(); err != nil {
		return err
	}
	logs.Info("webhook", "loaded agent identity agent_id=%s", server.GetAgentID())

	// Webhook events ride the existing gRPC application-event channel. The
	// global client is initialized but NOT started: this daemon must not open
	// a second MonitorStream alongside `phelix monitor` (duplicated metrics
	// streams). Events degrade to short-lived connections exactly like CLI
	// commands when no session is available.
	phelixgrpc.InitGlobalClient()

	queue := webhook.NewQueue(webhook.QueueOptions{
		Rebuild: webhook.NewCliRebuild().Rebuild,
		OnJobDone: func(job *webhook.Job, err error) {
			if err == nil {
				logs.Info("webhook", "rebuild completed app=%s delivery=%s branch=%s commit=%s",
					job.AppName, job.DeliveryID, job.Branch, job.CommitSHA)
				return
			}
			if webhook.IsShutdownErr(err) {
				logs.Warning("webhook", "rebuild left running at shutdown app=%s delivery=%s",
					job.AppName, job.DeliveryID)
				return
			}
			logs.Error("webhook", "rebuild trigger failed app=%s app_id=%s delivery=%s commit=%s: %v",
				job.AppName, job.AppID, job.DeliveryID, job.CommitSHA, err)
			webhook.ReportEvent(job.AppID, job.AppName, webhook.EventActionRebuildFailed, false, err.Error())
		},
		OnJobDropped: func(job *webhook.Job) {
			webhook.ReportEvent(job.AppID, job.AppName, webhook.EventActionQueueFailure, false,
				"server shut down before the job started")
		},
	})

	addr := fmt.Sprintf("%s:%d", webhookHost, webhookPort)
	srv := webhook.NewServer(webhook.ServerConfig{Addr: addr}, webhook.Dependencies{
		Resolver:     webhook.NewRegistryResolver(),
		Ledger:       ledger,
		Queue:        queue,
		LookupSecret: os.LookupEnv,
		ReportEvent:  webhook.ReportEvent,
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodePortUnavailable, err, "webhook: could not listen on %s", addr)
	}
	logs.Info("webhook", "webhook server listening on %s", addr)
	webhook.ReportEvent("", "", webhook.EventActionServerStart, true, "")

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigChan:
		logs.Info("webhook", "shutdown signal received (%v), stopping...", sig)
	case err := <-serveErr:
		return phelixerr.Wrapf(phelixerr.CodeServer, err, "webhook: server failed")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), webhookHTTPDrain)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logs.Warning("webhook", "http shutdown: %v", err)
	}
	queue.Close(webhookQueueDrain)
	logs.Info("webhook", "webhook server stopped")
	return nil
}
