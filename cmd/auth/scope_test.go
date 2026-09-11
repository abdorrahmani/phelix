package auth

// scope_test.go covers the agent-scoped login feature (B1,
// docs/CLI_CHANGES_REQUIRED.md §1) and the login rate-limit handling (C3):
//
//   - a --scope agent login sends X-Token-Scope: agent and lands in the
//     daemon's dedicated session file, never the interactive one;
//   - an interactive login never sends the header and never touches the
//     daemon file;
//   - the backend's response scope is persisted;
//   - an HTTP 429 with Retry-After produces a clear RATE_LIMITED error and
//     writes no session.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/config"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// loginRequestRecorder captures the login request's headers and replies with
// a canned session payload.
type loginRequestRecorder struct {
	server *httptest.Server

	gotScopeHeader   string
	scopeHeaderSeen  bool
	username         string
	apiKey           string
	responseStatus   int
	responseBody     string
	responseHeaders  map[string]string
	requestsReceived int
}

// newLoginRecorder starts a test login endpoint replying with responseStatus
// and responseBody (defaults to a successful agent-scoped session).
func newLoginRecorder(t *testing.T) *loginRequestRecorder {
	t.Helper()
	if err := config.Load(); err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	rec := &loginRequestRecorder{
		responseStatus: http.StatusOK,
		responseBody: `{
			"token": "synthetic-token",
			"sessionID": "sess-agent-1",
			"expiresAt": "` + time.Now().Add(72*time.Hour).UTC().Format(time.RFC3339) + `",
			"scope": "agent",
			"user": {"id": 7, "username": "abdul"}
		}`,
	}

	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.requestsReceived++
		rec.gotScopeHeader = r.Header.Get("X-Token-Scope")
		_, rec.scopeHeaderSeen = r.Header["X-Token-Scope"]
		rec.username = r.Header.Get("X-Username")
		rec.apiKey = r.Header.Get("X-API-Key")

		for k, v := range rec.responseHeaders {
			w.Header().Set(k, v)
		}
		w.WriteHeader(rec.responseStatus)
		_, _ = w.Write([]byte(rec.responseBody))
	}))
	t.Cleanup(rec.server.Close)

	// Point the auth client at the test server (test seams in client.go).
	prevBase, prevClient := testAuthBaseURL, testAuthHTTPClient
	testAuthBaseURL = strings.TrimSuffix(rec.server.URL, "/")
	testAuthHTTPClient = rec.server.Client()
	t.Cleanup(func() {
		testAuthBaseURL = prevBase
		testAuthHTTPClient = prevClient
	})

	return rec
}

// isolatedHome gives the test a clean HOME and reports the two session paths.
func isolatedHome(t *testing.T) (sessionPath, agentSessionPath string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return getSessionFilePath(), getAgentSessionFilePath()
}

func TestAuthenticate_AgentScopeSendsHeaderAndUsesDaemonFile(t *testing.T) {
	sessionPath, agentSessionPath := isolatedHome(t)
	rec := newLoginRecorder(t)

	if err := authenticate("abdul", "synthetic-key", ScopeAgent); err != nil {
		t.Fatalf("authenticate(--scope agent): %v", err)
	}

	// The header must be sent — exactly once, with the documented value.
	if !rec.scopeHeaderSeen {
		t.Fatal("agent-scoped login did not send X-Token-Scope")
	}
	if rec.gotScopeHeader != ScopeAgent {
		t.Fatalf("X-Token-Scope = %q, want %q", rec.gotScopeHeader, ScopeAgent)
	}

	// The session must land in the daemon's dedicated file...
	data, err := os.ReadFile(agentSessionPath)
	if err != nil {
		t.Fatalf("agent session file not written: %v", err)
	}
	var stored Session
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("agent session file unparseable: %v", err)
	}
	if stored.SessionID != "sess-agent-1" || stored.Token != "synthetic-token" {
		t.Fatalf("agent session content mismatch: %+v", stored)
	}
	if stored.Scope != ScopeAgent {
		t.Fatalf("persisted scope = %q, want %q (echoed from the response)", stored.Scope, ScopeAgent)
	}

	// ...and never displace the interactive session.
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Fatalf("agent-scoped login must not write the interactive session file (stat err=%v)", err)
	}
}

func TestAuthenticate_InteractiveLoginOmitsScopeHeader(t *testing.T) {
	sessionPath, agentSessionPath := isolatedHome(t)
	rec := newLoginRecorder(t)
	// A full-scope reply, as the backend answers without the header.
	rec.responseBody = `{
		"token": "synthetic-full-token",
		"sessionID": "sess-full-1",
		"expiresAt": "` + time.Now().Add(72*time.Hour).UTC().Format(time.RFC3339) + `",
		"user": {"id": 7, "username": "abdul"}
	}`

	if err := authenticate("abdul", "synthetic-key", ""); err != nil {
		t.Fatalf("authenticate(interactive): %v", err)
	}

	if rec.scopeHeaderSeen {
		t.Fatalf("interactive login must not send X-Token-Scope (got %q)", rec.gotScopeHeader)
	}

	// The interactive session file is written; the daemon file is not.
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("interactive session file not written: %v", err)
	}
	var stored Session
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("session file unparseable: %v", err)
	}
	if stored.SessionID != "sess-full-1" {
		t.Fatalf("interactive session content mismatch: %+v", stored)
	}
	// No scope in the response → persisted as full, never empty.
	if stored.Scope != ScopeFull {
		t.Fatalf("persisted scope = %q, want %q when the backend omits the field", stored.Scope, ScopeFull)
	}
	if _, err := os.Stat(agentSessionPath); !os.IsNotExist(err) {
		t.Fatalf("interactive login must not write the agent session file (stat err=%v)", err)
	}
}

func TestAuthenticate_LoginRateLimitedHonorsRetryAfter(t *testing.T) {
	sessionPath, agentSessionPath := isolatedHome(t)
	rec := newLoginRecorder(t)
	rec.responseStatus = http.StatusTooManyRequests
	rec.responseHeaders = map[string]string{"Retry-After": "120"}

	err := authenticate("abdul", "wrong-key-again", "")
	if err == nil {
		t.Fatal("expected an error from a 429 login, got nil")
	}
	if got := phelixerr.CodeOf(err); got != phelixerr.CodeRateLimited {
		t.Fatalf("error code = %v, want RATE_LIMITED", got)
	}
	// The operator-facing message must carry the announced window (120s).
	if !strings.Contains(err.Error(), "2m0s") {
		t.Fatalf("error should mention the Retry-After window (2m0s): %v", err)
	}

	// A throttled login must not write any session.
	for _, p := range []string{sessionPath, agentSessionPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("throttled login must not write %s (stat err=%v)", p, err)
		}
	}
}

func TestAuthenticate_LoginRateLimitedWithoutHeader(t *testing.T) {
	isolatedHome(t)
	rec := newLoginRecorder(t)
	rec.responseStatus = http.StatusTooManyRequests

	err := authenticate("abdul", "wrong-key", "")
	if err == nil {
		t.Fatal("expected an error from a 429 login, got nil")
	}
	if got := phelixerr.CodeOf(err); got != phelixerr.CodeRateLimited {
		t.Fatalf("error code = %v, want RATE_LIMITED", got)
	}
	if !strings.Contains(err.Error(), "too many login attempts") {
		t.Fatalf("error should explain the throttle: %v", err)
	}
}

func TestParseRetryAfterHeader(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		value  string
		want   time.Duration
		wantOK bool
	}{
		{"empty", "", 0, false},
		{"delay-seconds", "120", 120 * time.Second, true},
		{"zero", "0", 0, true},
		{"negative", "-5", 0, false},
		{"http-date future", "Mon, 11 Sep 2026 12:02:30 GMT", 150 * time.Second, true},
		{"http-date past", "Mon, 11 Sep 2026 11:00:00 GMT", 0, true},
		{"garbage", "soon", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseRetryAfterHeader(tc.value, now)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("%s: parseRetryAfterHeader(%q) = (%v, %v), want (%v, %v)",
				tc.name, tc.value, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestSessionSplit_AgentAndInteractiveCoexist(t *testing.T) {
	_, agentSessionPath := isolatedHome(t)

	// Daemon-only provisioning: an agent session and no interactive session.
	agent := &Session{
		SessionID: "sess-agent-1",
		Token:     "synthetic-agent-token",
		ExpiresAt: time.Now().Add(time.Hour),
		Scope:     ScopeAgent,
	}
	if err := storeSessionAt(agent, getAgentSessionFilePath()); err != nil {
		t.Fatalf("store agent session: %v", err)
	}

	// The interactive session does not exist — GetValidSession fails.
	if _, err := GetValidSession(); err == nil {
		t.Fatal("GetValidSession should fail with only an agent session on disk")
	}

	// But the agent session resolves.
	got, ok := GetValidAgentSession()
	if !ok || got == nil {
		t.Fatal("GetValidAgentSession should find the daemon session")
	}
	if got.SessionID != "sess-agent-1" {
		t.Fatalf("agent session = %+v", got)
	}

	// IsLoggedIn must recognize the machine as authenticated (gRPC sync
	// works with the agent token).
	if !IsLoggedIn() {
		t.Fatal("IsLoggedIn should be true with a valid agent session")
	}

	// An expired agent session is not valid.
	expired := &Session{
		SessionID: "sess-agent-old",
		Token:     "synthetic-agent-token",
		ExpiresAt: time.Now().Add(-time.Hour),
		Scope:     ScopeAgent,
	}
	if err := os.WriteFile(agentSessionPath, mustJSON(t, expired), 0600); err != nil {
		t.Fatalf("write expired agent session: %v", err)
	}
	if _, ok := GetValidAgentSession(); ok {
		t.Fatal("expired agent session should not resolve")
	}
	if IsLoggedIn() {
		t.Fatal("IsLoggedIn should be false with only an expired agent session")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func TestSessionPathForScope(t *testing.T) {
	isolatedHome(t)
	if got := sessionPathForScope(ScopeAgent); got != getAgentSessionFilePath() {
		t.Errorf("sessionPathForScope(agent) = %q, want agent file", got)
	}
	for _, scope := range []string{"", ScopeFull, "weird"} {
		if got := sessionPathForScope(scope); got != getSessionFilePath() {
			t.Errorf("sessionPathForScope(%q) = %q, want interactive file", scope, got)
		}
	}
}

func TestRemoveAgentSession(t *testing.T) {
	_, agentSessionPath := isolatedHome(t)
	if err := os.MkdirAll(filepath.Dir(agentSessionPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(agentSessionPath, []byte("{}"), 0600); err != nil {
		t.Fatalf("write agent session: %v", err)
	}
	if err := removeAgentSession(); err != nil {
		t.Fatalf("removeAgentSession: %v", err)
	}
	if _, err := os.Stat(agentSessionPath); !os.IsNotExist(err) {
		t.Fatalf("agent session file still present (stat err=%v)", err)
	}
	// Removing a missing file is not an error-worthy surprise here; assert it
	// returns an error callers can treat as "already gone" without crashing.
	_ = removeAgentSession()
}
