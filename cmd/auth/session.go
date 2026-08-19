package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

const sessionFileName = "session.json"

// ErrNotLoggedIn is returned by operations that would upload data to the
// Phelix dashboard when no valid session exists. It is informational — the
// caller should skip the upload, not fail the whole command.
var ErrNotLoggedIn = phelixerr.New(
	phelixerr.CodeUnauthenticated,
	"not logged in; skipping dashboard upload. Run 'phelix auth login' to enable monitoring",
)

// getSessionFilePath returns the full path of the session file.
func getSessionFilePath() string {
	return filepath.Join(os.Getenv("HOME"), ".phelix", sessionFileName)
}

// readSession loads the session from disk.
func readSession() (*Session, error) {
	path := getSessionFilePath()
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

// storeSession saves the session to disk.
func storeSession(session *Session) error {
	sessionDir := filepath.Join(os.Getenv("HOME"), ".phelix")
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

	return os.WriteFile(getSessionFilePath(), data, 0600)
}

// removeSession deletes the session file.
func removeSession() error {
	return os.Remove(getSessionFilePath())
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

// GetValidSession returns a valid, non-expired session.
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

// IsLoggedIn reports whether a valid, non-expired session exists on disk. It
// never fails — a missing or expired session simply means the CLI should run
// in offline mode (build/run works, but metrics are not sent to the
// dashboard until the user runs 'phelix auth login').
func IsLoggedIn() bool {
	_, err := GetValidSession()
	return err == nil
}
