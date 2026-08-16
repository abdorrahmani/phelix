package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

const sessionFileName = "session.json"

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
func storeSession(sessionID, token string) error {
	sessionDir := filepath.Join(os.Getenv("HOME"), ".phelix")
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return phelixerr.Wrapf(
			phelixerr.CodeFilesystem,
			err,
			"error creating session directory %s",
			sessionDir,
		)
	}

	session := Session{
		SessionID: sessionID,
		Token:     token,
		ExpiresAt: time.Now().Add(time.Hour * 24 * 30 * 3),
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
