package auth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/abdorrahmani/gophel/config"
)

// authenticate performs login with the given credentials.
func authenticate(username, apiKey string) error {
	cfg := config.Get()
	req, err := http.NewRequest("POST", cfg.App.API+"/gophel/auth", nil)
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("X-Username", username)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("invalid credentials: status code %d", resp.StatusCode)
	}

	var result struct {
		Session string `json:"sessionID"`
		Token   string `json:"token"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("error decoding response: %w", err)
	}

	return storeSession(result.Session, result.Token)
}

// VerifySession checks if the session is still valid.
func VerifySession(session *Session) error {
	cfg := config.Get()
	req, err := http.NewRequest("GET", cfg.App.API+"gophel/auth/status", nil)
	if err != nil {
		return err
	}

	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("Authorization", "Bearer "+session.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("error verifying session: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("invalid session")
	}

	return nil
}

// performLogout invalidates the current session on the server.
func performLogout(session *Session) error {
	cfg := config.Get()
	req, err := http.NewRequest("POST", cfg.App.API+"/gophel/auth/logout", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("Authorization", "Bearer "+session.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("error sending logout request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("logout failed: %s", string(body))
	}
	return nil
}
