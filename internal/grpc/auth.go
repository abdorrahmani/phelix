package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"google.golang.org/grpc/metadata"
)

// sessionData holds the session information from disk.
type sessionData struct {
	SessionID string    `json:"sessionID"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// loadSession reads the session from $HOME/.phelix/session.json.
func loadSession() (*sessionData, error) {
	path := filepath.Join(os.Getenv("HOME"), ".phelix", "session.json")
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
