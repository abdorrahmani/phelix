package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Session scope values persisted in the session file's "scope" field and
// shown by `phelix auth status`. An empty/missing field is a full-scope
// session (every session issued before the scope feature existed).
const (
	// ScopeAgent is a restricted token issued via `phelix auth login --scope
	// agent`: accepted by the agent gRPC surface and only two REST endpoints
	// (auth status/logout), never by dashboard/account APIs. This is the
	// scope the monitor daemon's session should use — see
	// docs/CLI_CHANGES_REQUIRED.md §1 (B1).
	ScopeAgent = "agent"
	// ScopeFull is an unrestricted session (the default for interactive
	// logins; the only scope that may be used for REST-calling commands).
	ScopeFull = "full"
)

const (
	sessionFileName      = "session.json"
	agentSessionFileName = "agent-session.json"
)

// ErrNotLoggedIn is returned by operations that would upload data to the
// Phelix dashboard when no valid session exists. It is informational — the
// caller should skip the upload, not fail the whole command.
var ErrNotLoggedIn = phelixerr.New(
	phelixerr.CodeUnauthenticated,
	"not logged in; skipping dashboard upload. Run 'phelix auth login' to enable monitoring",
)

// getSessionFilePath returns the full path of the interactive session file.
func getSessionFilePath() string {
	return filepath.Join(os.Getenv("HOME"), ".phelix", sessionFileName)
}

// getAgentSessionFilePath returns the full path of the monitor daemon's
// dedicated agent-scoped session file. Kept separate from session.json so an
// agent-scoped login never displaces the full-scope session interactive
// commands rely on (its token is rejected by dashboard REST endpoints).
func getAgentSessionFilePath() string {
	return filepath.Join(os.Getenv("HOME"), ".phelix", agentSessionFileName)
}

// sessionPathForScope maps a requested login scope to the file it is stored
// in: agent-scoped sessions go to the daemon's dedicated file, everything
// else to the interactive session file.
func sessionPathForScope(scope string) string {
	if scope == ScopeAgent {
		return getAgentSessionFilePath()
	}
	return getSessionFilePath()
}

// readSession loads the interactive session from disk.
func readSession() (*Session, error) {
	return readSessionFile(getSessionFilePath())
}

// readAgentSession loads the daemon's agent-scoped session from disk.
func readAgentSession() (*Session, error) {
	return readSessionFile(getAgentSessionFilePath())
}

// readSessionFile loads and parses a single session file.
func readSessionFile(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}

	return &session, nil
}

// storeSessionAt writes the session to the given path.
func storeSessionAt(session *Session, path string) error {
	sessionDir := filepath.Dir(path)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return phelixerr.Wrapf(
			phelixerr.CodeFilesystem,
			err,
			"error creating session directory %s",
			sessionDir,
		)
	}

	data, err := json.Marshal(session)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "error serializing session", err)
	}

	return os.WriteFile(path, data, 0600)
}

// storeSession saves the interactive session to disk.
func storeSession(session *Session) error {
	return storeSessionAt(session, getSessionFilePath())
}

// removeSession deletes the interactive session file.
func removeSession() error {
	return os.Remove(getSessionFilePath())
}

// removeAgentSession deletes the daemon's agent-scoped session file.
func removeAgentSession() error {
	return os.Remove(getAgentSessionFilePath())
}

// defaultSessionTTL is the fallback lifetime when the server response omits
// expiresAt. The documented default expiry is ~3 months.
const defaultSessionTTL = 24 * 30 * 3 * time.Hour

// parseExpiry parses the RFC 3339 expiresAt value returned by the backend. An
// empty value is tolerated and falls back to the default TTL.
func parseExpiry(s string) (time.Time, error) {
	if s == "" {
		return time.Now().Add(defaultSessionTTL), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

// GetValidSession returns a valid, non-expired interactive session. REST-
// calling commands (app sync, session management) must use this one: an
// agent-scoped token would be rejected with 403 by every dashboard endpoint.
func GetValidSession() (*Session, error) {
	session, err := readSession()
	if err != nil {
		return nil, phelixerr.New(
			phelixerr.CodeUnauthenticated,
			"no active session found. Please run 'phelix auth login' first",
		)
	}

	if time.Now().After(session.ExpiresAt) {
		return nil, phelixerr.New(
			phelixerr.CodeSessionExpired,
			"session has expired. Please re-authenticate",
		)
	}
	return session, nil
}

// GetValidAgentSession returns the daemon's agent-scoped session when one
// exists and is unexpired. The gRPC client prefers it when present (it
// authenticates the whole agent surface); the boolean reports whether the
// dedicated session was found so callers can fall back.
func GetValidAgentSession() (*Session, bool) {
	session, err := readAgentSession()
	if err != nil || time.Now().After(session.ExpiresAt) {
		return nil, false
	}
	return session, true
}

// IsLoggedIn reports whether a valid, non-expired session exists on disk. It
// never fails — a missing or expired session simply means the CLI should run
// in offline mode (build/run works, but metrics are not sent to the
// dashboard until the user runs 'phelix auth login').
//
// Either session counts: a machine set up with only `phelix auth login
// --scope agent` (daemon-only provisioning) still has working gRPC sync.
func IsLoggedIn() bool {
	if _, err := GetValidSession(); err == nil {
		return true
	}
	_, ok := GetValidAgentSession()
	return ok
}
