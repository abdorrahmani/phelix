// Package webhook implements the Git-push webhook server: it authenticates
// deliveries (HMAC-SHA256 over the raw body), validates them against the
// app's phelix.yaml webhook section, deduplicates redeliveries through a
// durable ledger, and hands accepted jobs to a per-app build queue. The queue
// triggers rebuilds exclusively through the existing `phelix rebuild`
// pipeline — this package owns no build, deploy, health, version or rollback
// logic, and it never acquires or bypasses the per-app deploy lock.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
)

const (
	// webhookPathPrefix is the endpoint prefix: POST /webhook/<app>.
	webhookPathPrefix = "/webhook/"

	// DefaultMaxBodyBytes caps the request body (GitHub push payloads are
	// typically well under 100 KiB; 2 MiB leaves generous headroom). The cap
	// is enforced on the bytes actually read, never on a client-supplied
	// Content-Length.
	DefaultMaxBodyBytes = 2 << 20
)

// ServerConfig holds the webhook server's listening parameters.
type ServerConfig struct {
	Addr         string
	MaxBodyBytes int64
}

// Dependencies are the server's collaborators; all injectable for tests.
type Dependencies struct {
	// Resolver maps the path's <app> to a managed application and its
	// webhook configuration. Required.
	Resolver AppResolver
	// Ledger durably records accepted deliveries for replay protection.
	// Required.
	Ledger *DeliveryLedger
	// Queue accepts jobs for serialized rebuild execution. Required.
	Queue *Queue
	// Jobs is the durable deployment-job store. Required for acceptance:
	// a delivery is only acknowledged once its job record is persisted.
	Jobs *JobStore
	// LookupSecret reads a named secret environment variable. Defaults to a
	// function that never finds one (tests inject os.LookupEnv or fakes).
	LookupSecret func(string) (string, bool)
	// ReportEvent forwards webhook events to the backend. Defaults to the
	// gRPC application-event reporter.
	ReportEvent func(appID, appName, action string, success bool, errMsg string)
}

// Server is the webhook HTTP server.
type Server struct {
	cfg  ServerConfig
	deps Dependencies
	http *http.Server
}

// NewServer builds the server around its dependencies.
func NewServer(cfg ServerConfig, deps Dependencies) *Server {
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if deps.LookupSecret == nil {
		deps.LookupSecret = func(string) (string, bool) { return "", false }
	}
	if deps.ReportEvent == nil {
		deps.ReportEvent = ReportEvent
	}
	s := &Server{cfg: cfg, deps: deps}
	mux := http.NewServeMux()
	mux.HandleFunc(webhookPathPrefix, s.handleWebhook)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.writeResponse(w, http.StatusNotFound, false, false, "not found")
	})
	s.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return s
}

// Handler exposes the HTTP handler (tests mount it on httptest servers).
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Serve accepts connections on ln until Shutdown is called.
func (s *Server) Serve(ln net.Listener) error { return s.http.Serve(ln) }

// Shutdown gracefully drains in-flight HTTP requests. The build queue has its
// own Close.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

// Response is the uniform JSON body of every webhook endpoint reply. It never
// carries internal errors, secrets, filesystem paths or stack traces.
type Response struct {
	Accepted bool   `json:"accepted"`
	Queued   bool   `json:"queued"`
	Message  string `json:"message"`
}

func (s *Server) writeResponse(w http.ResponseWriter, status int, accepted, queued bool, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Response{Accepted: accepted, Queued: queued, Message: message})
}

// handleWebhook implements POST /webhook/<app>. The pipeline, in order:
// resolve the app (404 for unknown apps and disabled webhooks) → read the
// secret → bound the body → verify the HMAC over the raw bytes (401) →
// parse the payload → ignore non-matching branches → deduplicate the
// delivery → enqueue the job and return immediately (the rebuild runs in the
// queue, never in the handler).
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeResponse(w, http.StatusMethodNotAllowed, false, false, "method not allowed")
		return
	}

	identifier := strings.TrimPrefix(r.URL.Path, webhookPathPrefix)
	if identifier == "" || strings.ContainsAny(identifier, "/\\") {
		s.writeResponse(w, http.StatusNotFound, false, false, "unknown application")
		return
	}

	resolved, err := s.deps.Resolver.Resolve(identifier)
	if err != nil {
		if phelixerr.IsCode(err, phelixerr.CodeNotFound) {
			s.writeResponse(w, http.StatusNotFound, false, false, "unknown application")
			return
		}
		logs.Error("webhook", "could not resolve app %q: %v", identifier, err)
		s.writeResponse(w, http.StatusInternalServerError, false, false, "webhook unavailable")
		return
	}
	if resolved.Config == nil || !resolved.Config.Enabled {
		// The endpoint is not active for this app. Answer exactly like an
		// unknown app so unauthenticated callers learn nothing more.
		s.writeResponse(w, http.StatusNotFound, false, false, "unknown application")
		return
	}

	secret, ok := s.deps.LookupSecret(resolved.Config.SecretEnv)
	if !ok || strings.TrimSpace(secret) == "" {
		// Startup validation should have caught this; keep the request-time
		// guard for apps registered while the server runs. The env var name
		// stays out of the response (unauthenticated caller).
		logs.Error("webhook", "app %s: webhook secret environment variable %s is not set",
			resolved.Info.Name, resolved.Config.SecretEnv)
		s.writeResponse(w, http.StatusInternalServerError, false, false, "webhook unavailable")
		return
	}

	if r.ContentLength > s.cfg.MaxBodyBytes {
		s.writeResponse(w, http.StatusRequestEntityTooLarge, false, false, "request body too large")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeResponse(w, http.StatusRequestEntityTooLarge, false, false, "request body too large")
			return
		}
		s.writeResponse(w, http.StatusBadRequest, false, false, "could not read request body")
		return
	}

	// Authenticate the exact raw bytes before trusting anything in them.
	deliveryID := strings.TrimSpace(r.Header.Get(DeliveryHeader))
	if !VerifySignature([]byte(secret), body, r.Header.Get(SignatureHeader)) {
		// Local log only: unauthenticated traffic must not trigger
		// authenticated backend event sends.
		logs.Warning("webhook", "rejected webhook app=%s delivery=%s remote=%s: invalid signature",
			resolved.Info.Name, deliveryID, r.RemoteAddr)
		s.writeResponse(w, http.StatusUnauthorized, false, false, "invalid signature")
		return
	}

	payload, err := ParsePushPayload(body)
	if err != nil {
		logs.Warning("webhook", "rejected webhook app=%s delivery=%s: unparseable payload", resolved.Info.Name, deliveryID)
		s.deps.ReportEvent(resolved.Info.ID, resolved.Info.Name, EventActionRejected, false, "unparseable payload")
		s.writeResponse(w, http.StatusBadRequest, false, false, "invalid payload")
		return
	}
	if deliveryID == "" {
		// Without a delivery identifier there is no replay protection, so the
		// request is refused rather than processed undeduplicated.
		logs.Warning("webhook", "rejected webhook app=%s: missing %s header", resolved.Info.Name, DeliveryHeader)
		s.deps.ReportEvent(resolved.Info.ID, resolved.Info.Name, EventActionRejected, false, "missing delivery id")
		s.writeResponse(w, http.StatusBadRequest, false, false, "missing delivery id")
		return
	}

	eventType := r.Header.Get(EventTypeHeader)
	branch := BranchFromRef(payload.Ref)

	// Branch mismatch is not a server error: the push is authentic, it just
	// targets a branch that does not trigger this app. Same for provider
	// handshake events (ping) and branch deletions — accepted, never queued.
	if ignored := ignoredReason(eventType, payload, branch, resolved.Config.Branch); ignored != "" {
		logs.Info("webhook", "ignored webhook app=%s delivery=%s branch=%q commit=%s: %s (configured branch %q)",
			resolved.Info.Name, deliveryID, branch, payload.After, ignored, resolved.Config.Branch)
		s.deps.ReportEvent(resolved.Info.ID, resolved.Info.Name, EventActionBranchMismatch, true, ignored)
		s.writeResponse(w, http.StatusOK, true, false, ignored)
		return
	}

	seen, err := s.deps.Ledger.SeenOrRecord(resolved.Info.Name, deliveryID, branch, payload.After, ProviderGitHub)
	if err != nil {
		logs.Error("webhook", "app %s delivery=%s: delivery ledger failed: %v", resolved.Info.Name, deliveryID, err)
		s.writeResponse(w, http.StatusInternalServerError, false, false, "webhook unavailable")
		return
	}
	if seen {
		logs.Info("webhook", "duplicate delivery app=%s delivery=%s branch=%s commit=%s",
			resolved.Info.Name, deliveryID, branch, payload.After)
		s.deps.ReportEvent(resolved.Info.ID, resolved.Info.Name, EventActionDuplicate, true, "")
		s.writeResponse(w, http.StatusOK, true, false, "delivery already processed")
		return
	}

	// The durable deployment job is created and persisted BEFORE the delivery
	// is acknowledged: if this write fails, the request is refused (and the
	// ledger entry rolled back) so the provider redelivers — GitHub must
	// never be told "accepted" while Phelix holds no durable record of the
	// job.
	if s.deps.Jobs == nil {
		logs.Error("webhook", "app %s delivery=%s: no durable job store configured", resolved.Info.Name, deliveryID)
		s.writeResponse(w, http.StatusInternalServerError, false, false, "webhook unavailable")
		return
	}
	now := time.Now().UnixMilli()
	record := &JobRecord{
		ID:         NewJobID(),
		DeliveryID: deliveryID,
		AppID:      resolved.Info.ID,
		AppName:    resolved.Info.Name,
		Branch:     branch,
		Commit:     payload.After,
		Provider:   ProviderGitHub,
		Status:     StatusAccepted,
		Stage:      StageQueue,
		AcceptedAt: now,
	}
	if err := s.deps.Jobs.Create(record); err != nil {
		logs.Error("webhook", "app %s delivery=%s: could not persist deployment job: %v",
			resolved.Info.Name, deliveryID, err)
		if rmErr := s.deps.Ledger.Remove(resolved.Info.Name, deliveryID); rmErr != nil {
			logs.Error("webhook", "app %s delivery=%s: could not roll back ledger entry: %v",
				resolved.Info.Name, deliveryID, rmErr)
		}
		s.writeResponse(w, http.StatusInternalServerError, false, false, "webhook unavailable")
		return
	}

	job := &Job{
		ID:         record.ID,
		AppID:      resolved.Info.ID,
		AppName:    resolved.Info.Name,
		Directory:  resolved.Info.Directory,
		Branch:     branch,
		CommitSHA:  payload.After,
		DeliveryID: deliveryID,
		Provider:   ProviderGitHub,
		ReceivedAt: time.Now(),
	}
	if err := s.deps.Queue.Enqueue(job); err != nil {
		// The job never started; forget the delivery so the provider's
		// redelivery (triggered by this non-2xx) starts from a clean slate,
		// and close the job record as a queue failure.
		if rmErr := s.deps.Ledger.Remove(resolved.Info.Name, deliveryID); rmErr != nil {
			logs.Error("webhook", "app %s delivery=%s: could not roll back ledger entry: %v",
				resolved.Info.Name, deliveryID, rmErr)
		}
		if mfErr := s.deps.Jobs.MarkFailed(record.ID, JobErrQueue, err.Error()); mfErr != nil {
			logs.Warning("webhook", "job %s: could not record queue failure: %v", record.ID, mfErr)
		}
		logs.Error("webhook", "queue failure app=%s job=%s delivery=%s commit=%s: %v",
			resolved.Info.Name, record.ID, deliveryID, payload.After, err)
		s.deps.ReportEvent(resolved.Info.ID, resolved.Info.Name, EventActionQueueFailure, false, err.Error())
		s.writeResponse(w, http.StatusServiceUnavailable, false, false, "webhook queue unavailable")
		return
	}
	if qErr := s.deps.Jobs.MarkQueued(record.ID); qErr != nil {
		// The job IS queued and will run; a failed status write is logged,
		// never used to fail an accepted delivery.
		logs.Warning("webhook", "job %s: could not record queued state: %v", record.ID, qErr)
	}

	logs.Info("webhook", "accepted webhook app=%s app_id=%s job=%s delivery=%s branch=%s commit=%s provider=%s",
		resolved.Info.Name, resolved.Info.ID, record.ID, deliveryID, branch, payload.After, ProviderGitHub)
	s.deps.ReportEvent(resolved.Info.ID, resolved.Info.Name, EventActionAccepted, true, JobEventMessage(record))
	s.writeResponse(w, http.StatusOK, true, true, "webhook accepted")
}

// ignoredReason reports why an authenticated push should NOT trigger a
// rebuild, or "" when it should. Only the configured branch (after ref
// normalization) triggers; a deleted branch has nothing to rebuild; non-push
// events (ping) are provider handshakes.
func ignoredReason(eventType string, payload *PushPayload, branch, configured string) string {
	if eventType != "" && eventType != "push" {
		return "event ignored"
	}
	if IsBranchDeletion(payload) {
		return "branch deleted; nothing to rebuild"
	}
	if branch != configured {
		return "branch does not match configured branch"
	}
	return ""
}
