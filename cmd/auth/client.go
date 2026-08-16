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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "error sending request", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return phelixerr.Newf(
			phelixerr.CodeInvalidCredentials,
			"invalid credentials: status code %d",
			resp.StatusCode,
		)
	}

	var result struct {
		Session string `json:"sessionID"`
		Token   string `json:"token"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "error decoding response", err)
	}

	return storeSession(result.Session, result.Token)
}

// VerifySession checks if the session is still valid.
func VerifySession(session *Session) error {
	cfg := config.Get()
	req, err := http.NewRequest("GET", cfg.App.API+"/auth/phelix/status", nil)
	if err != nil {
		return err
	}

	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("Authorization", "Bearer "+session.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "error verifying session", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return phelixerr.New(phelixerr.CodeSessionExpired, "invalid session")
	}

	return nil
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
