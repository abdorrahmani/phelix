package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Session scope values persisted in the session file's "scope" field. An
// empty/missing field is a full-scope session (all sessions issued before
// the scope feature existed were full-scope).
const (
	// SessionScopeAgent is a restricted token issued via
	// `phelix auth login --scope agent`: it is accepted by the agent gRPC
	// surface only, never by account/dashboard REST APIs.
	SessionScopeAgent = "agent"
	// SessionScopeFull is an unrestricted session (the default).
	SessionScopeFull = "full"
)

// agentSessionFileName is the daemon's dedicated session file. It is kept
// separate from the interactive "session.json" so an agent-scoped login
// (whose token is rejected by dashboard/account REST endpoints) never
// displaces the full-scope session interactive commands rely on. See
// docs/CLI_CHANGES_REQUIRED.md §1.
const agentSessionFileName = "agent-session.json"

// sessionData holds the session information from disk.
type sessionData struct {
	SessionID string    `json:"sessionID"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	Scope     string    `json:"scope,omitempty"`
}

// sessionFilePath returns the path of the interactive session file.
func sessionFilePath() string {
	return filepath.Join(os.Getenv("HOME"), ".phelix", "session.json")
}

// agentSessionFilePath returns the path of the monitor daemon's dedicated
// (agent-scoped) session file.
func agentSessionFilePath() string {
	return filepath.Join(os.Getenv("HOME"), ".phelix", agentSessionFileName)
}

// loadSession returns the session the gRPC client should authenticate with:
// the daemon's agent-scoped session when present, otherwise the interactive
// full-scope session (the pre-agent-scope behavior, kept so existing
// deployments keep working until they adopt --scope agent).
func loadSession() (*sessionData, error) {
	if s, err := readSessionFile(agentSessionFilePath()); err == nil {
		return s, nil
	}
	return readSessionFile(sessionFilePath())
}

// readSessionFile reads and validates a single session file. A missing or
// expired agent session is not fatal — loadSession falls back to the
// interactive session — so the distinction is only meaningful to the caller
// through the error's code.
func readSessionFile(path string) (*sessionData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, phelixerr.New(
				phelixerr.CodeUnauthenticated,
				"no active session found. Please run 'phelix auth login' first",
			)
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read session file %s", path)
	}

	var session sessionData
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to parse session file", err)
	}

	if time.Now().After(session.ExpiresAt) {
		return nil, phelixerr.New(
			phelixerr.CodeSessionExpired,
			"session has expired. Please re-authenticate",
		)
	}

	return &session, nil
}

// sessionAvailable reports whether a valid, non-expired session exists. When
// it returns false, the CLI runs in offline mode: commands still work, but
// nothing is uploaded to the dashboard until the user runs 'phelix auth login'.
// This check is intentionally quiet — no error is logged.
func sessionAvailable() bool {
	_, err := loadSession()
	return err == nil
}

// PreferredSessionScope returns the scope of the session the gRPC client
// would authenticate with ("" when no valid session exists). The monitor
// daemon uses it to warn when it is running on a full-scope token instead of
// an agent-scoped one (audit B1).
func PreferredSessionScope() string {
	if s, err := loadSession(); err == nil {
		return s.Scope
	}
	return ""
}

// attachAuthMetadata creates a new context with authentication metadata.
// Sends multiple header formats to ensure the backend can find the session
// and token regardless of which header names it checks.
func attachAuthMetadata(ctx context.Context, serverID string) (context.Context, error) {
	session, err := loadSession()
	if err != nil {
		return nil, err
	}

	md := metadata.New(map[string]string{
		"sessionid":     session.SessionID,
		"token":         session.Token,
		"authorization": "Bearer " + session.Token,
		"serverid":      serverID,
		"x-server-id":   serverID,
		"server_id":     serverID,
	})

	return metadata.NewOutgoingContext(ctx, md), nil
}

// isAuthRejection reports whether err is (or wraps) a gRPC Unauthenticated
// status — i.e. the backend actively rejected the presented credentials
// (revoked or invalid session), as opposed to a local failure such as "no
// session file" or a network error. status.FromError traverses wrapped error
// chains, so this also works on errors already wrapped by phelixerr.Wrap.
func isAuthRejection(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := status.FromError(err); ok {
		return st.Code() == codes.Unauthenticated
	}
	return false
}

// ---------------------------------------------------------------------------
// Backend error-shape classification (2026-09-11 hardening, §2 E1–E5)
// ---------------------------------------------------------------------------

// Backend rate-limit / abuse-resistance errors, matched by substring on the
// gRPC status message (the backend's contract per CLI_CHANGES_REQUIRED.md).
const (
	msgFailedAuthBudget = "too many failed authentication attempts"
	msgStreamRateBudget = "message rate exceeded"
	msgStreamCap        = "too many concurrent agent streams"
	msgStreamIdle       = "stream idle timeout"
)

// grpcStatusMessage extracts the message of the deepest gRPC status error in
// err's chain. status.FromError cannot be used for this: on a wrapped error
// it preserves the code but replaces the message with the outer wrapper's
// text — which hides the backend's budget markers ("message rate exceeded",
// "too many concurrent agent streams", ...) that classification depends on.
func grpcStatusMessage(err error) (string, bool) {
	var (
		msg   string
		found bool
	)
	for e := err; e != nil; e = errors.Unwrap(e) {
		if gs, ok := e.(interface{ GRPCStatus() *status.Status }); ok {
			if st := gs.GRPCStatus(); st != nil {
				msg = st.Message()
				found = true
			}
		}
	}
	return msg, found
}

// resourceExhaustedKind classifies a gRPC ResourceExhausted error by the
// backend budget it came from. Returns "" when err is not a matching
// ResourceExhausted status. Works through phelixerr wraps: the code comes
// from status.FromError (which preserves it), the marker text from the
// original status message (see grpcStatusMessage).
func resourceExhaustedKind(err error) string {
	if err == nil {
		return ""
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted {
		return ""
	}
	msg := strings.ToLower(st.Message())
	if raw, ok := grpcStatusMessage(err); ok {
		msg += " " + strings.ToLower(raw)
	}
	switch {
	case strings.Contains(msg, msgFailedAuthBudget):
		return "auth_budget"
	case strings.Contains(msg, msgStreamRateBudget):
		return "stream_rate"
	case strings.Contains(msg, msgStreamCap):
		return "stream_cap"
	default:
		// A ResourceExhausted without a known budget marker is treated as a
		// generic send failure — most plausibly the 4 MiB MaxRecvMsgSize
		// (E4: a message too large to be accepted).
		return "send_limit"
	}
}

// isStreamIdleTimeout reports whether err is (or wraps) the backend's
// DeadlineExceeded "stream idle timeout" (E5): the stream was reaped for
// inactivity. Functionally identical to a network disconnect — the caller
// reconnects with the normal backoff.
func isStreamIdleTimeout(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.DeadlineExceeded {
		return false
	}
	if strings.Contains(strings.ToLower(st.Message()), msgStreamIdle) {
		return true
	}
	msg, ok := grpcStatusMessage(err)
	return ok && strings.Contains(strings.ToLower(msg), msgStreamIdle)
}
