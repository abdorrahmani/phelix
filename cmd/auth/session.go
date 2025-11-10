package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const sessionFileName = "session.json"

// getSessionFilePath returns the full path of the session file.
func getSessionFilePath() string {
	return filepath.Join(os.Getenv("HOME"), ".gophel", sessionFileName)
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
	sessionDir := filepath.Join(os.Getenv("HOME"), ".gophel")
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return fmt.Errorf("error creating session directory %s: %w", sessionDir, err)
	}

	session := Session{
		SessionID: sessionID,
		Token:     token,
		ExpiresAt: time.Now().Add(time.Hour * 24 * 30 * 3),
	}

	data, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("error serializing session: %w", err)
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
		return nil, fmt.Errorf("no active session found. Please run 'gophel auth' first")
	}

	if time.Now().After(session.ExpiresAt) {
		return nil, fmt.Errorf("session has expired. Please re-authenticate")
	}
	return session, nil
}
