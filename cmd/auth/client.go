package auth

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/abdorrahmani/phelix/config"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Test seams: the login/verify/logout HTTP surface is otherwise pinned to the
// embedded production config, which would make the request-shape behavior
// (headers, 429 handling) untestable. Both default to nil/"" in production.
var (
	// testAuthBaseURL, when non-empty, overrides cfg.App.API.
	testAuthBaseURL string
	// testAuthHTTPClient, when non-nil, replaces http.DefaultClient.
	testAuthHTTPClient *http.Client
)

// apiBase returns the backend REST base URL for the auth endpoints.
func apiBase(cfg *config.Config) string {
	if testAuthBaseURL != "" {
		return testAuthBaseURL
	}
	return cfg.App.API
}

// doAuthRequest sends req with the production (or test-injected) HTTP client.
func doAuthRequest(req *http.Request) (*http.Response, error) {
	client := http.DefaultClient
	if testAuthHTTPClient != nil {
		client = testAuthHTTPClient
	}
	return client.Do(req)
}

// authenticate performs login with the given credentials.
//
// scope requests a scoped token: ScopeAgent sends `X-Token-Scope: agent` so
// the backend issues a monitoring-only token (B1). Any other value (including
// "") performs a normal full-scope login — interactive commands that call
// REST APIs depend on that, so the header must never leak into the default
// path.
func authenticate(username, apiKey, scope string) error {
	cfg := config.Get()
	if cfg == nil {
		return phelixerr.New(phelixerr.CodeConfiguration, "no configuration loaded")
	}
	req, err := http.NewRequest("POST", apiBase(cfg)+"/auth/phelix", nil)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "error creating request", err)
	}

	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("X-Username", username)
	for k, v := range authHeaders() {
		req.Header.Set(k, v)
	}
	// Agent-scoped login (B1): only for the monitor daemon's session. Never
	// set for interactive logins — an agent token is rejected by dashboard
	// REST endpoints.
	if scope == ScopeAgent {
		req.Header.Set("X-Token-Scope", ScopeAgent)
	}

	resp, err := doAuthRequest(req)
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
	case http.StatusTooManyRequests:
		// C3: the backend throttles failed logins. Honor the announced
		// window and tell the operator plainly, instead of surfacing a
		// generic failure they would be tempted to re-run immediately
		// (which would only extend the throttle).
		return loginRateLimitedError(resp)
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

	// The persisted scope is the backend's answer (additive field): it may
	// be empty on older backends even when we requested agent scope.
	scopeVal := result.Scope
	if scopeVal == "" {
		scopeVal = ScopeFull
	}

	return storeSessionAt(&Session{
		SessionID: result.SessionID,
		Token:     result.Token,
		Username:  result.User.Username,
		UserID:    result.User.ID,
		ExpiresAt: expiresAt,
		Scope:     scopeVal,
	}, sessionPathForScope(scope))
}

// loginRateLimitedError converts an HTTP 429 login response into a clear
// operator-facing error. The Retry-After header (seconds, or an HTTP-date) is
// honored when present; without it, a generic throttle message with a
// conservative hint is returned.
func loginRateLimitedError(resp *http.Response) error {
	if after, ok := parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now()); ok {
		return phelixerr.Newf(
			phelixerr.CodeRateLimited,
			"too many login attempts: the backend asks to retry after %s. Wait for the window to pass before trying again",
			after.Round(time.Second),
		)
	}
	return phelixerr.New(
		phelixerr.CodeRateLimited,
		"too many login attempts: the backend is throttling logins. Wait a few minutes before trying again",
	)
}

// parseRetryAfterHeader parses a Retry-After header value, which per RFC 9110
// is either delay-seconds or an HTTP-date.
func parseRetryAfterHeader(v string, now time.Time) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// authResponse mirrors the login endpoint's JSON payload. Only the fields the
// CLI needs are kept; the user object's sensitive fields are never rendered.
type authResponse struct {
	Token     string   `json:"token"`
	SessionID string   `json:"sessionID"`
	ExpiresAt string   `json:"expiresAt"`
	Scope     string   `json:"scope"`
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
	if cfg == nil {
		return nil, phelixerr.New(phelixerr.CodeConfiguration, "no configuration loaded")
	}
	req, err := http.NewRequest("GET", apiBase(cfg)+"/auth/phelix/status", nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("Authorization", "Bearer "+session.Token)

	resp, err := doAuthRequest(req)
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
	if cfg == nil {
		return phelixerr.New(phelixerr.CodeConfiguration, "no configuration loaded")
	}
	req, err := http.NewRequest("POST", apiBase(cfg)+"/auth/phelix/logout", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Session-ID", session.SessionID)
	req.Header.Set("Authorization", "Bearer "+session.Token)

	resp, err := doAuthRequest(req)
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
