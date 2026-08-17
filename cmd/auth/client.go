package auth

import (
	"encoding/json"
	"net/http"

	"github.com/abdorrahmani/phelix/config"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// authenticate performs login with the given credentials.
func authenticate(username, apiKey string) error {
	cfg := config.Get()
	req, err := http.NewRequest("POST", cfg.App.API+"/auth/phelix", nil)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "error creating request", err)
	}

	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("X-Username", username)
	for k, v := range authHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "error sending request", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return phelixerr.New(
			phelixerr.CodeInvalidCredentials,
			"invalid credentials: unknown username or API key mismatch",
		)
	case http.StatusBadRequest:
		return phelixerr.Newf(
			phelixerr.CodeInvalidArgument,
			"authentication request rejected: server returned HTTP %d",
			resp.StatusCode,
		)
	case http.StatusOK:
	default:
		return phelixerr.Newf(
			phelixerr.CodeServer,
			"authentication failed: server returned HTTP %d",
			resp.StatusCode,
		)
	}

	// The response may carry session tokens; the body is never echoed anywhere,
	// only the parsed fields below are retained.
	var result authResponse

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "error decoding response", err)
	}

	expiresAt, err := parseExpiry(result.ExpiresAt)
	if err != nil {
		return phelixerr.Wrap(
			phelixerr.CodeServer,
			"error decoding session expiry from response",
			err,
		)
	}

	return storeSession(&Session{
		SessionID: result.SessionID,
		Token:     result.Token,
		Username:  result.User.Username,
		UserID:    result.User.ID,
		ExpiresAt: expiresAt,
	})
}

// authResponse mirrors the login endpoint's JSON payload. Only the fields the
// CLI needs are kept; the user object's sensitive fields are never rendered.
type authResponse struct {
	Token     string   `json:"token"`
	SessionID string   `json:"sessionID"`
	ExpiresAt string   `json:"expiresAt"`
	User      authUser `json:"user"`
}

// authUser is the subset of the user object sent back on login.
type authUser struct {
	ID       uint   `json:"id"`
	Username string `json:"username"`
}

// VerifySession checks if the session is still valid and returns the session
// status reported by the backend.
func VerifySession(session *Session) (*SessionStatus, error) {
	cfg := config.Get()
	req, err := http.NewRequest("GET", cfg.App.API+"/auth/phelix/status", nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("Authorization", "Bearer "+session.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeNetwork, "error verifying session", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, phelixerr.New(phelixerr.CodeSessionExpired, "invalid session")
	}

	var status SessionStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeNetwork, "error decoding session status", err)
	}

	return &status, nil
}

// performLogout invalidates the current session on the server.
func performLogout(session *Session) error {
	cfg := config.Get()
	req, err := http.NewRequest("POST", cfg.App.API+"/auth/phelix/logout", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("Authorization", "Bearer "+session.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "error sending logout request", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The response body is intentionally NOT embedded: it may contain
		// session tokens or stack traces. Only the status is surfaced.
		return phelixerr.Newf(
			phelixerr.CodeNetwork,
			"logout failed: server returned HTTP %d",
			resp.StatusCode,
		)
	}
	return nil
}
