package webhook

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/project"
)

const testSecret = "test-webhook-secret"

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type stubResolver struct {
	apps map[string]*ResolvedApp
}

func (r *stubResolver) Resolve(identifier string) (*ResolvedApp, error) {
	if a, ok := r.apps[identifier]; ok {
		return a, nil
	}
	return nil, phelixerr.Newf(phelixerr.CodeNotFound, "unknown application %q", identifier)
}

type capturedEvent struct {
	appID, appName, action string
	success                bool
	errMsg                 string
}

type eventCapture struct {
	mu     sync.Mutex
	events []capturedEvent
}

func (c *eventCapture) record(appID, appName, action string, success bool, errMsg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, capturedEvent{appID, appName, action, success, errMsg})
}

func (c *eventCapture) actions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.events))
	for i, e := range c.events {
		out[i] = e.action
	}
	return out
}

func (c *eventCapture) hasAction(action string) bool {
	for _, a := range c.actions() {
		if a == action {
			return true
		}
	}
	return false
}

// fakeRebuild records the jobs it executes; optional gating, sleeping and
// failure injection support the concurrency and response-latency tests.
type fakeRebuild struct {
	mu        sync.Mutex
	jobs      []*Job
	err       error
	sleep     time.Duration
	gate      chan struct{} // when non-nil, every job blocks until released
	started   chan struct{}
	active    int32
	maxActive int32
}

func newFakeRebuild() *fakeRebuild {
	return &fakeRebuild{started: make(chan struct{}, 64)}
}

func (f *fakeRebuild) Rebuild(_ context.Context, job *Job) error {
	cur := atomic.AddInt32(&f.active, 1)
	for {
		m := atomic.LoadInt32(&f.maxActive)
		if cur <= m || atomic.CompareAndSwapInt32(&f.maxActive, m, cur) {
			break
		}
	}
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	f.mu.Unlock()
	f.started <- struct{}{}

	if f.gate != nil {
		<-f.gate
	}
	if f.sleep > 0 {
		time.Sleep(f.sleep)
	}
	atomic.AddInt32(&f.active, -1)

	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeRebuild) jobCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.jobs)
}

func (f *fakeRebuild) recorded() []*Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*Job, len(f.jobs))
	copy(out, f.jobs)
	return out
}

func (f *fakeRebuild) waitJobs(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if f.jobCount() >= n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d jobs, got %d", n, f.jobCount())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	t        *testing.T
	url      string
	queue    *Queue
	ledger   *DeliveryLedger
	jobs     *JobStore
	rebuild  *fakeRebuild
	events   *eventCapture
	secrets  map[string]string
	resolver *stubResolver
}

func enabledApp(name, id, branch, secretEnv string) *ResolvedApp {
	return &ResolvedApp{
		Info:   AppInfo{ID: id, Name: name, Directory: filepath.Join(os.TempDir(), "phelix-webhook-test", name)},
		Config: &project.WebhookConfig{Enabled: true, Branch: branch, SecretEnv: secretEnv},
	}
}

// newHarness serves one app named "api" (ID "app-1") on branch main with
// PHELIX_WEBHOOK_SECRET, plus any extra apps.
func newHarness(t *testing.T, extra ...*ResolvedApp) *harness {
	t.Helper()

	apps := append([]*ResolvedApp{enabledApp("api", "app-1", "main", "PHELIX_WEBHOOK_SECRET")}, extra...)
	resolver := &stubResolver{apps: map[string]*ResolvedApp{}}
	for _, a := range apps {
		resolver.apps[a.Info.Name] = a
		resolver.apps[a.Info.ID] = a
	}

	ledger := NewDeliveryLedger(filepath.Join(t.TempDir(), "deliveries.json"), DefaultLedgerCapacity)
	if err := ledger.Initialize(); err != nil {
		t.Fatalf("ledger init: %v", err)
	}

	jobs := NewJobStore(filepath.Join(t.TempDir(), "jobs"), DefaultJobRetention)
	if err := jobs.Load(); err != nil {
		t.Fatalf("job store init: %v", err)
	}

	rebuild := newFakeRebuild()
	queue := NewQueue(QueueOptions{Rebuild: rebuild.Rebuild})
	events := &eventCapture{}
	secrets := map[string]string{"PHELIX_WEBHOOK_SECRET": testSecret}

	srv := NewServer(ServerConfig{}, Dependencies{
		Resolver:     resolver,
		Ledger:       ledger,
		Queue:        queue,
		Jobs:         jobs,
		LookupSecret: func(name string) (string, bool) { v, ok := secrets[name]; return v, ok },
		ReportEvent:  events.record,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		queue.Close(2 * time.Second)
	})

	return &harness{
		t:        t,
		url:      ts.URL,
		queue:    queue,
		ledger:   ledger,
		jobs:     jobs,
		rebuild:  rebuild,
		events:   events,
		secrets:  secrets,
		resolver: resolver,
	}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func pushBody(t *testing.T, ref, after string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]string{"ref": ref, "before": "1111111111111111111111111111111111111111", "after": after})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

const testCommit = "5f0d1c2b7a9e4f8c3d6b1a2c3d4e5f60718293a4"

// post sends a signed push webhook for app and returns the HTTP status and
// decoded response body.
func (h *harness) post(t *testing.T, app, secret string, body []byte, delivery string, mutate func(*http.Request)) (int, Response) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.url+"/webhook/"+app, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, sign(secret, body))
	req.Header.Set(DeliveryHeader, delivery)
	req.Header.Set(EventTypeHeader, "push")
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	var out Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, out
}

func (h *harness) postPush(t *testing.T, app, delivery, ref string) (int, Response) {
	t.Helper()
	return h.post(t, app, testSecret, pushBody(t, ref, testCommit), delivery, nil)
}

func wantResponse(t *testing.T, got Response, accepted, queued bool, message string) {
	t.Helper()
	if got.Accepted != accepted || got.Queued != queued || got.Message != message {
		t.Fatalf("response = %+v, want {accepted:%v queued:%v message:%q}", got, accepted, queued, message)
	}
}

func wantStatus(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

// ---------------------------------------------------------------------------
// HMAC verification (unit)
// ---------------------------------------------------------------------------

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	valid := sign(testSecret, body)

	cases := []struct {
		name   string
		secret string
		body   []byte
		header string
		want   bool
	}{
		{"valid", testSecret, body, valid, true},
		{"wrong secret", "other-secret", body, valid, false},
		{"modified body", testSecret, []byte(`{"ref":"refs/heads/dev"}`), valid, false},
		{"missing header", testSecret, body, "", false},
		{"empty header", testSecret, body, "sha256=", false},
		{"wrong prefix", testSecret, body, "md5=" + strings.TrimPrefix(valid, "sha256="), false},
		{"no prefix", testSecret, body, strings.TrimPrefix(valid, "sha256="), false},
		{"short digest", testSecret, body, "sha256=abcd", false},
		{"wrong length digest", testSecret, body, "sha256=" + strings.Repeat("a", 63), false},
		{"non-hex digest", testSecret, body, "sha256=" + "z" + strings.TrimPrefix(valid, "sha256=abcd")[1:], false},
		{"uppercase hex of same digest", testSecret, body, "sha256=" + strings.ToUpper(strings.TrimPrefix(valid, "sha256=")), true},
		{"empty secret", "", body, valid, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifySignature([]byte(tc.secret), tc.body, tc.header); got != tc.want {
				t.Fatalf("VerifySignature = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Authentication (handler)
// ---------------------------------------------------------------------------

func TestWebhookHandler_Authentication(t *testing.T) {
	t.Run("valid HMAC accepted", func(t *testing.T) {
		h := newHarness(t)
		status, resp := h.postPush(t, "api", "d-auth-1", "refs/heads/main")
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, true, "webhook accepted")
	})

	t.Run("invalid HMAC rejected without queue job", func(t *testing.T) {
		h := newHarness(t)
		status, resp := h.post(t, "api", "wrong-secret", pushBody(t, "refs/heads/main", testCommit), "d-auth-2", nil)
		wantStatus(t, status, http.StatusUnauthorized)
		wantResponse(t, resp, false, false, "invalid signature")
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("invalid signature created %d queue jobs", n)
		}
		if h.events.hasAction(EventActionAccepted) {
			t.Fatal("invalid signature must not emit an accepted event")
		}
	})

	t.Run("missing signature rejected", func(t *testing.T) {
		h := newHarness(t)
		body := pushBody(t, "refs/heads/main", testCommit)
		status, resp := h.post(t, "api", testSecret, body, "d-auth-3", func(r *http.Request) {
			r.Header.Del(SignatureHeader)
		})
		wantStatus(t, status, http.StatusUnauthorized)
		wantResponse(t, resp, false, false, "invalid signature")
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("missing signature created %d queue jobs", n)
		}
	})

	malformed := map[string]string{
		"empty":            "sha256=",
		"garbage":          "not-a-signature",
		"wrong scheme":     "sha1=deadbeef",
		"odd-length hex":   "sha256=abc",
		"non-hex chars":    "sha256=" + strings.Repeat("g", 64),
		"truncated digest": "sha256=" + strings.Repeat("a", 31),
	}
	for name, header := range malformed {
		t.Run("malformed signature rejected: "+name, func(t *testing.T) {
			h := newHarness(t)
			body := pushBody(t, "refs/heads/main", testCommit)
			status, resp := h.post(t, "api", testSecret, body, "d-mal-"+name, func(r *http.Request) {
				r.Header.Set(SignatureHeader, header)
			})
			wantStatus(t, status, http.StatusUnauthorized)
			wantResponse(t, resp, false, false, "invalid signature")
			if n := h.rebuild.jobCount(); n != 0 {
				t.Fatalf("malformed signature created %d queue jobs", n)
			}
		})
	}

	t.Run("modified body fails authentication", func(t *testing.T) {
		h := newHarness(t)
		signed := pushBody(t, "refs/heads/main", testCommit)
		sent := pushBody(t, "refs/heads/dev", testCommit) // same length, different branch
		status, resp := h.post(t, "api", testSecret, sent, "d-auth-4", func(r *http.Request) {
			r.Header.Set(SignatureHeader, sign(testSecret, signed))
		})
		wantStatus(t, status, http.StatusUnauthorized)
		wantResponse(t, resp, false, false, "invalid signature")
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("modified body created %d queue jobs", n)
		}
	})

	t.Run("exact raw body is used for HMAC", func(t *testing.T) {
		h := newHarness(t)
		// Raw bytes with trailing whitespace: the signature must be computed
		// over exactly these bytes, not a trimmed or re-encoded copy.
		raw := append(pushBody(t, "refs/heads/main", testCommit), []byte("  \n")...)
		status, resp := h.post(t, "api", testSecret, raw, "d-auth-5", nil)
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, true, "webhook accepted")

		// And a signature over the trimmed body must NOT authenticate the
		// untrimmed one.
		trimmed := pushBody(t, "refs/heads/main", testCommit)
		status, resp = h.post(t, "api", testSecret, raw, "d-auth-6", func(r *http.Request) {
			r.Header.Set(SignatureHeader, sign(testSecret, trimmed))
		})
		wantStatus(t, status, http.StatusUnauthorized)
		wantResponse(t, resp, false, false, "invalid signature")
	})
}

// ---------------------------------------------------------------------------
// Branch handling
// ---------------------------------------------------------------------------

func TestWebhookHandler_Branch(t *testing.T) {
	t.Run("refs/heads/main accepted", func(t *testing.T) {
		h := newHarness(t)
		status, resp := h.postPush(t, "api", "d-br-1", "refs/heads/main")
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, true, "webhook accepted")
		h.rebuild.waitJobs(t, 1)
	})

	t.Run("bare main accepted", func(t *testing.T) {
		h := newHarness(t)
		status, resp := h.postPush(t, "api", "d-br-2", "main")
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, true, "webhook accepted")
		h.rebuild.waitJobs(t, 1)
	})

	t.Run("different branch ignored, no queue job", func(t *testing.T) {
		h := newHarness(t)
		status, resp := h.postPush(t, "api", "d-br-3", "refs/heads/dev")
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, false, "branch does not match configured branch")
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("branch mismatch created %d queue jobs", n)
		}
		if !h.events.hasAction(EventActionBranchMismatch) {
			t.Fatal("expected a branch-mismatch event")
		}
	})

	t.Run("tag push never matches a branch", func(t *testing.T) {
		h := newHarness(t)
		status, resp := h.postPush(t, "api", "d-br-4", "refs/tags/main")
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, false, "branch does not match configured branch")
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("tag push created %d queue jobs", n)
		}
	})

	t.Run("multi-segment branch", func(t *testing.T) {
		h := newHarness(t)
		h.resolver.apps["api"] = enabledApp("api", "app-1", "release/2.x", "PHELIX_WEBHOOK_SECRET")
		status, resp := h.postPush(t, "api", "d-br-5", "refs/heads/release/2.x")
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, true, "webhook accepted")
		h.rebuild.waitJobs(t, 1)
	})

	t.Run("branch deletion ignored", func(t *testing.T) {
		h := newHarness(t)
		status, resp := h.post(t, "api", testSecret,
			pushBody(t, "refs/heads/main", strings.Repeat("0", 40)), "d-br-6", nil)
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, false, "branch deleted; nothing to rebuild")
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("branch deletion created %d queue jobs", n)
		}
	})

	t.Run("non-push event (ping) ignored", func(t *testing.T) {
		h := newHarness(t)
		body := []byte(`{"zen":"Keep it simple."}`)
		status, resp := h.post(t, "api", testSecret, body, "d-br-7", func(r *http.Request) {
			r.Header.Set(EventTypeHeader, "ping")
		})
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, false, "event ignored")
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("ping created %d queue jobs", n)
		}
	})
}

// ---------------------------------------------------------------------------
// Application resolution
// ---------------------------------------------------------------------------

func TestWebhookHandler_Application(t *testing.T) {
	t.Run("known app by name", func(t *testing.T) {
		h := newHarness(t)
		status, _ := h.postPush(t, "api", "d-app-1", "refs/heads/main")
		wantStatus(t, status, http.StatusOK)
		h.rebuild.waitJobs(t, 1)
	})

	t.Run("known app by ID", func(t *testing.T) {
		h := newHarness(t)
		status, resp := h.postPush(t, "app-1", "d-app-2", "refs/heads/main")
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, true, "webhook accepted")
		h.rebuild.waitJobs(t, 1)
	})

	t.Run("unknown app 404, no queue job", func(t *testing.T) {
		h := newHarness(t)
		status, resp := h.postPush(t, "no-such-app", "d-app-3", "refs/heads/main")
		wantStatus(t, status, http.StatusNotFound)
		wantResponse(t, resp, false, false, "unknown application")
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("unknown app created %d queue jobs", n)
		}
	})

	t.Run("webhook disabled (no section) 404, no queue job", func(t *testing.T) {
		h := newHarness(t)
		off := enabledApp("web", "app-2", "main", "PHELIX_WEBHOOK_SECRET")
		off.Config = nil
		h.resolver.apps["web"] = off
		h.resolver.apps["app-2"] = off

		status, resp := h.postPush(t, "web", "d-app-4", "refs/heads/main")
		wantStatus(t, status, http.StatusNotFound)
		wantResponse(t, resp, false, false, "unknown application")
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("disabled webhook created %d queue jobs", n)
		}
	})

	t.Run("webhook disabled (enabled false) 404, no queue job", func(t *testing.T) {
		h := newHarness(t)
		off := enabledApp("web", "app-2", "main", "PHELIX_WEBHOOK_SECRET")
		off.Config.Enabled = false
		h.resolver.apps["web"] = off
		h.resolver.apps["app-2"] = off

		status, resp := h.postPush(t, "web", "d-app-5", "refs/heads/main")
		wantStatus(t, status, http.StatusNotFound)
		wantResponse(t, resp, false, false, "unknown application")
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("disabled webhook created %d queue jobs", n)
		}
	})

	t.Run("missing secret 500, no queue job", func(t *testing.T) {
		h := newHarness(t)
		delete(h.secrets, "PHELIX_WEBHOOK_SECRET")
		status, resp := h.postPush(t, "api", "d-app-6", "refs/heads/main")
		wantStatus(t, status, http.StatusInternalServerError)
		wantResponse(t, resp, false, false, "webhook unavailable")
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("missing secret created %d queue jobs", n)
		}
	})
}

// ---------------------------------------------------------------------------
// Request shape / limits
// ---------------------------------------------------------------------------

func TestWebhookHandler_RequestShape(t *testing.T) {
	t.Run("non-POST method 405", func(t *testing.T) {
		h := newHarness(t)
		resp, err := http.Get(h.url + "/webhook/api")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		wantStatus(t, resp.StatusCode, http.StatusMethodNotAllowed)
	})

	unknownPath := func(t *testing.T, h *harness, path string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.url+path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		wantStatus(t, resp.StatusCode, http.StatusNotFound)
	}
	t.Run("root path 404", func(t *testing.T) {
		h := newHarness(t)
		unknownPath(t, h, "/webhook")
		unknownPath(t, h, "/webhook/")
	})
	t.Run("nested path 404", func(t *testing.T) {
		h := newHarness(t)
		unknownPath(t, h, "/webhook/api/extra")
	})
	t.Run("unrelated path 404", func(t *testing.T) {
		h := newHarness(t)
		unknownPath(t, h, "/other")
	})

	t.Run("oversized Content-Length rejected before reading", func(t *testing.T) {
		h := newHarness(t)
		// Raw request: the Go client refuses to send a Content-Length that
		// exceeds the body, so verify the header check at the protocol level.
		host := strings.TrimPrefix(h.url, "http://")
		conn, err := net.DialTimeout("tcp", host, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		fmt.Fprintf(conn, "POST /webhook/api HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n%s: sha256=%s\r\n\r\n",
			host, DefaultMaxBodyBytes+1, SignatureHeader, strings.Repeat("a", 64))
		statusLine, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(statusLine, fmt.Sprintf("%d", http.StatusRequestEntityTooLarge)) {
			t.Fatalf("status line = %q, want %d", statusLine, http.StatusRequestEntityTooLarge)
		}
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("oversized request created %d queue jobs", n)
		}
	})

	t.Run("oversized streamed body rejected", func(t *testing.T) {
		h := newHarness(t)
		// Chunked body (no Content-Length): MaxBytesReader must enforce the
		// limit on the bytes actually read.
		big := make([]byte, DefaultMaxBodyBytes+4096)
		for i := range big {
			big[i] = 'a'
		}
		req, err := http.NewRequest(http.MethodPost, h.url+"/webhook/api", bytes.NewReader(big))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(SignatureHeader, sign(testSecret, big))
		req.Header.Set(DeliveryHeader, "d-big-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		wantStatus(t, resp.StatusCode, http.StatusRequestEntityTooLarge)
	})

	t.Run("invalid payload 400", func(t *testing.T) {
		h := newHarness(t)
		body := []byte("this is not json")
		status, resp := h.post(t, "api", testSecret, body, "d-bad-1", nil)
		wantStatus(t, status, http.StatusBadRequest)
		wantResponse(t, resp, false, false, "invalid payload")
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("invalid payload created %d queue jobs", n)
		}
	})

	t.Run("missing delivery id 400", func(t *testing.T) {
		h := newHarness(t)
		body := pushBody(t, "refs/heads/main", testCommit)
		status, resp := h.post(t, "api", testSecret, body, "", func(r *http.Request) {
			r.Header.Del(DeliveryHeader)
		})
		wantStatus(t, status, http.StatusBadRequest)
		wantResponse(t, resp, false, false, "missing delivery id")
		if n := h.rebuild.jobCount(); n != 0 {
			t.Fatalf("missing delivery id created %d queue jobs", n)
		}
	})
}

// ---------------------------------------------------------------------------
// Delivery deduplication
// ---------------------------------------------------------------------------

func TestWebhookHandler_Dedup(t *testing.T) {
	t.Run("first delivery queued", func(t *testing.T) {
		h := newHarness(t)
		status, resp := h.postPush(t, "api", "d-dup-1", "refs/heads/main")
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, true, "webhook accepted")
		h.rebuild.waitJobs(t, 1)
	})

	t.Run("same delivery twice queues exactly one job", func(t *testing.T) {
		h := newHarness(t)
		status, resp := h.postPush(t, "api", "d-dup-2", "refs/heads/main")
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, true, "webhook accepted")

		status, resp = h.postPush(t, "api", "d-dup-2", "refs/heads/main")
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, false, "delivery already processed")

		h.rebuild.waitJobs(t, 1)
		if n := h.rebuild.jobCount(); n != 1 {
			t.Fatalf("duplicate delivery created %d jobs, want 1", n)
		}
		if !h.events.hasAction(EventActionDuplicate) {
			t.Fatal("expected a duplicate-delivery event")
		}
	})

	t.Run("different delivery ids are independent", func(t *testing.T) {
		h := newHarness(t)
		h.postPush(t, "api", "d-dup-3", "refs/heads/main")
		h.postPush(t, "api", "d-dup-4", "refs/heads/main")
		h.rebuild.waitJobs(t, 2)
	})

	t.Run("concurrent duplicate deliveries create exactly one job", func(t *testing.T) {
		h := newHarness(t)
		const n = 8
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make([]Response, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, results[i] = h.postPush(t, "api", "d-dup-conc", "refs/heads/main")
			}(i)
		}
		close(start)
		wg.Wait()

		h.rebuild.waitJobs(t, 1)
		if got := h.rebuild.jobCount(); got != 1 {
			t.Fatalf("concurrent duplicates created %d jobs, want 1", got)
		}
		queued := 0
		for _, r := range results {
			if r.Queued {
				queued++
			}
			if !r.Accepted {
				t.Fatalf("concurrent duplicate not accepted: %+v", r)
			}
		}
		if queued != 1 {
			t.Fatalf("%d responses reported queued, want exactly 1", queued)
		}
	})

	t.Run("delivery survives a server restart (durable ledger)", func(t *testing.T) {
		// Simulate a restart: same ledger file, fresh server.
		dir := t.TempDir()
		path := filepath.Join(dir, "deliveries.json")
		l1 := NewDeliveryLedger(path, DefaultLedgerCapacity)
		if err := l1.Initialize(); err != nil {
			t.Fatal(err)
		}
		if seen, err := l1.SeenOrRecord("api", "d-restart", "main", testCommit, ProviderGitHub); err != nil || seen {
			t.Fatalf("first record: seen=%v err=%v", seen, err)
		}
		l2 := NewDeliveryLedger(path, DefaultLedgerCapacity)
		if err := l2.Initialize(); err != nil {
			t.Fatal(err)
		}
		seen, err := l2.SeenOrRecord("api", "d-restart", "main", testCommit, ProviderGitHub)
		if err != nil || !seen {
			t.Fatalf("after restart: seen=%v err=%v (want seen=true)", seen, err)
		}
	})

	t.Run("failed enqueue rolls the delivery back", func(t *testing.T) {
		h := newHarness(t)
		// A closed queue makes Enqueue fail; the ledger entry must be removed
		// so the provider's redelivery can be processed.
		h.queue.Close(0)

		status, resp := h.postPush(t, "api", "d-roll-1", "refs/heads/main")
		wantStatus(t, status, http.StatusServiceUnavailable)
		wantResponse(t, resp, false, false, "webhook queue unavailable")
		if !h.events.hasAction(EventActionQueueFailure) {
			t.Fatal("expected a queue-failure event")
		}

		// New queue on the same ledger: the redelivery must be accepted.
		rebuild := newFakeRebuild()
		queue := NewQueue(QueueOptions{Rebuild: rebuild.Rebuild})
		t.Cleanup(func() { queue.Close(2 * time.Second) })
		srv := NewServer(ServerConfig{}, Dependencies{
			Resolver:     h.resolver,
			Ledger:       h.ledger,
			Queue:        queue,
			Jobs:         h.jobs,
			LookupSecret: func(name string) (string, bool) { v, ok := h.secrets[name]; return v, ok },
			ReportEvent:  h.events.record,
		})
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()

		body := pushBody(t, "refs/heads/main", testCommit)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhook/api", bytes.NewReader(body))
		req.Header.Set(SignatureHeader, sign(testSecret, body))
		req.Header.Set(DeliveryHeader, "d-roll-1")
		req.Header.Set(EventTypeHeader, "push")
		httpResp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer httpResp.Body.Close()
		var out Response
		_ = json.NewDecoder(httpResp.Body).Decode(&out)
		if httpResp.StatusCode != http.StatusOK || !out.Queued {
			t.Fatalf("redelivery after queue failure: status=%d resp=%+v", httpResp.StatusCode, out)
		}
		rebuild.waitJobs(t, 1)
	})
}

// ---------------------------------------------------------------------------
// Queue behavior
// ---------------------------------------------------------------------------

func TestWebhookHandler_ResponseDoesNotWaitForRebuild(t *testing.T) {
	h := newHarness(t)
	h.rebuild.gate = make(chan struct{})
	defer close(h.rebuild.gate)

	done := make(chan struct{})
	go func() {
		defer close(done)
		status, resp := h.postPush(t, "api", "d-fast-1", "refs/heads/main")
		wantStatus(t, status, http.StatusOK)
		wantResponse(t, resp, true, true, "webhook accepted")
	}()

	select {
	case <-done:
		// Response returned while the rebuild is still blocked: good.
	case <-time.After(2 * time.Second):
		t.Fatal("handler waited for the rebuild to finish")
	}
	if n := h.rebuild.jobCount(); n != 1 {
		t.Fatalf("expected the job to be handed to the queue, got %d", n)
	}
}

func TestQueue_SameAppJobsExecuteSequentially(t *testing.T) {
	rebuild := newFakeRebuild()
	rebuild.sleep = 30 * time.Millisecond
	queue := NewQueue(QueueOptions{Rebuild: rebuild.Rebuild})
	t.Cleanup(func() { queue.Close(2 * time.Second) })

	for i := 0; i < 3; i++ {
		job := &Job{AppName: "api", AppID: "app-1", DeliveryID: fmt.Sprintf("d-seq-%d", i), Branch: "main"}
		if err := queue.Enqueue(job); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	rebuild.waitJobs(t, 3)

	if got := atomic.LoadInt32(&rebuild.maxActive); got != 1 {
		t.Fatalf("same-app jobs overlapped: max concurrent = %d, want 1", got)
	}
	if n := rebuild.jobCount(); n != 3 {
		t.Fatalf("executed %d jobs, want 3", n)
	}
}

func TestQueue_DifferentAppsExecuteIndependently(t *testing.T) {
	rebuild := newFakeRebuild()
	gateA := make(chan struct{})
	rebuild.gate = gateA

	queue := NewQueue(QueueOptions{Rebuild: rebuild.Rebuild})
	t.Cleanup(func() { queue.Close(2 * time.Second) })
	gateAClosed := false
	defer func() {
		if !gateAClosed {
			close(gateA)
		}
	}()

	// App "a" blocks inside its rebuild; app "b" must still run.
	if err := queue.Enqueue(&Job{AppName: "a", AppID: "a-1", DeliveryID: "d-ind-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rebuild.started:
	case <-time.After(2 * time.Second):
		t.Fatal("app a job never started")
	}

	if err := queue.Enqueue(&Job{AppName: "b", AppID: "b-1", DeliveryID: "d-ind-2"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rebuild.started:
		// App b started while app a is still blocked.
	case <-time.After(2 * time.Second):
		t.Fatal("app b job did not start while app a was blocked — apps are not independent")
	}
	gateAClosed = true
	close(gateA)
	rebuild.waitJobs(t, 2)
}

func TestQueue_JobPreservesPushMetadata(t *testing.T) {
	h := newHarness(t)
	h.postPush(t, "api", "d-meta-1", "refs/heads/main")
	h.rebuild.waitJobs(t, 1)

	job := h.rebuild.recorded()[0]
	if job.DeliveryID != "d-meta-1" {
		t.Fatalf("job delivery = %q, want d-meta-1", job.DeliveryID)
	}
	if job.CommitSHA != testCommit {
		t.Fatalf("job commit = %q, want %q", job.CommitSHA, testCommit)
	}
	if job.Branch != "main" {
		t.Fatalf("job branch = %q, want main", job.Branch)
	}
	if job.AppID != "app-1" || job.AppName != "api" {
		t.Fatalf("job app = %s/%s, want app-1/api", job.AppID, job.AppName)
	}
	if job.Provider != ProviderGitHub {
		t.Fatalf("job provider = %q, want %q", job.Provider, ProviderGitHub)
	}
	if job.ReceivedAt.IsZero() {
		t.Fatal("job has no received timestamp")
	}
}

func TestQueue_EnqueueFull(t *testing.T) {
	rebuild := newFakeRebuild()
	gate := make(chan struct{})
	rebuild.gate = gate
	queue := NewQueue(QueueOptions{Rebuild: rebuild.Rebuild})
	// Cleanups are LIFO: open the gate before closing the queue so the
	// blocked worker can finish and Close does not wait out its timeout.
	t.Cleanup(func() { queue.Close(5 * time.Second) })
	t.Cleanup(func() { close(gate) })

	// Occupy the worker first, then fill the per-app backlog to capacity.
	if err := queue.Enqueue(&Job{AppName: "api", AppID: "app-1", DeliveryID: "d-full-0"}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	select {
	case <-rebuild.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first job never started")
	}
	for i := 1; i <= perAppQueueDepth; i++ {
		if err := queue.Enqueue(&Job{AppName: "api", AppID: "app-1", DeliveryID: fmt.Sprintf("d-full-%d", i)}); err != nil {
			t.Fatalf("enqueue %d should fit in the backlog: %v", i, err)
		}
	}

	err := queue.Enqueue(&Job{AppName: "api", AppID: "app-1", DeliveryID: "d-full-overflow"})
	if err == nil {
		t.Fatalf("expected a queue-full error with the backlog full (%d pending)", perAppQueueDepth)
	}
	if !phelixerr.IsCode(err, phelixerr.CodeUnavailable) {
		t.Fatalf("queue-full error = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Rebuild service: existing pipeline + deploy lock authority
// ---------------------------------------------------------------------------

// prepareRecorder is an injectable SourcePreparer: it hands out a fixed
// source directory (or a failure) and records prepare/cleanup calls.
type prepareRecorder struct {
	mu      sync.Mutex
	dir     string
	err     error
	jobs    []*Job
	cleaned int32
}

func (r *prepareRecorder) preparer() SourcePreparer {
	return func(ctx context.Context, job *Job) (*PreparedSource, error) {
		r.mu.Lock()
		r.jobs = append(r.jobs, job)
		r.mu.Unlock()
		if r.err != nil {
			return nil, r.err
		}
		return &PreparedSource{
			Dir: r.dir,
			remove: func() error {
				atomic.AddInt32(&r.cleaned, 1)
				return nil
			},
		}, nil
	}
}

func (r *prepareRecorder) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.jobs)
}

func (r *prepareRecorder) cleanupCount() int32 { return atomic.LoadInt32(&r.cleaned) }

// recordingRunner captures the invocation without spawning a process.
func recordingRunner(invocations *sync.Map) commandRunner {
	return func(ctx context.Context, job *Job, dir string, args ...string) ([]string, error) {
		invocations.Store(dir, args)
		return nil, nil
	}
}

func lockTestJob() *Job {
	return &Job{
		AppID:      "app-1",
		AppName:    "api",
		Directory:  "/srv/api",
		Branch:     "main",
		CommitSHA:  testCommit,
		DeliveryID: "d-lock-1",
	}
}

const fakeSourceDir = "/srv/api-isolated"

func wantRebuildArgs(sourceDir string) []string {
	return []string{"rebuild", "app-1", "--source-dir", sourceDir}
}

func assertArgs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("invocation args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("invocation args = %v, want %v", got, want)
		}
	}
}

func TestCliRebuild_WaitsForDeployLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // deploy lock paths live under $HOME/.phelix

	var invocations sync.Map
	prep := &prepareRecorder{dir: fakeSourceDir}
	rb := &CliRebuild{poll: 5 * time.Millisecond, run: recordingRunner(&invocations), prepare: prep.preparer()}

	// A manual rebuild owns the deploy lock; the webhook job must wait — the
	// isolated source is prepared, but the rebuild itself must not start.
	release, err := deploy.AcquireDeployLock("api", "rebuild")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- rb.Rebuild(context.Background(), lockTestJob()) }()

	select {
	case err := <-done:
		t.Fatalf("rebuild returned while lock held: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if n := prep.callCount(); n != 1 {
		t.Fatalf("source prepared %d times, want 1", n)
	}
	if _, ran := invocations.Load("/srv/api"); ran {
		t.Fatal("rebuild invoked while the deploy lock was held by another operation")
	}

	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("rebuild after lock release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rebuild did not proceed after the lock was released")
	}

	args, _ := invocations.Load("/srv/api")
	assertArgs(t, args.([]string), wantRebuildArgs(fakeSourceDir))
	if n := prep.cleanupCount(); n != 1 {
		t.Fatalf("isolated source cleaned %d times, want exactly 1", n)
	}
}

func TestCliRebuild_RetriesAfterLockRace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var calls int32
	var mu sync.Mutex
	prep := &prepareRecorder{dir: fakeSourceDir}
	rb := &CliRebuild{
		poll:    5 * time.Millisecond,
		prepare: prep.preparer(),
		run: func(ctx context.Context, job *Job, dir string, args ...string) ([]string, error) {
			n := atomic.AddInt32(&calls, 1)
			mu.Lock()
			defer mu.Unlock()
			if n == 1 {
				// The rebuild lost the race for the lock (another deploy took
				// it between the probe and the subprocess acquire).
				return []string{"→ could not acquire deploy lock: api already has a \"deploy\" operation in progress"},
					phelixerr.Newf(phelixerr.CodeProcessFailed, "webhook: rebuild command failed")
			}
			return nil, nil
		},
	}

	if err := rb.Rebuild(context.Background(), lockTestJob()); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("runner calls = %d, want 2 (one lost race + one retry)", got)
	}
	if n := prep.cleanupCount(); n != 1 {
		t.Fatalf("isolated source cleaned %d times, want exactly 1 (retries share it)", n)
	}
}

func TestCliRebuild_BuildFailureIsNotRetried(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var calls int32
	prep := &prepareRecorder{dir: fakeSourceDir}
	rb := &CliRebuild{
		poll:    5 * time.Millisecond,
		prepare: prep.preparer(),
		run: func(ctx context.Context, job *Job, dir string, args ...string) ([]string, error) {
			atomic.AddInt32(&calls, 1)
			return []string{"→ build failed: exit status 1"},
				phelixerr.Newf(phelixerr.CodeProcessFailed, "webhook: rebuild command failed")
		},
	}

	if err := rb.Rebuild(context.Background(), lockTestJob()); err == nil {
		t.Fatal("expected the build failure to surface")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("runner calls = %d, want 1 (no retry on a genuine failure)", got)
	}
	if n := prep.cleanupCount(); n != 1 {
		t.Fatalf("isolated source cleaned %d times after a failed build, want 1", n)
	}
}

func TestCliRebuild_ShutdownWhileWaitingForLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// A rollback owns the deploy lock; the webhook job must keep waiting (and
	// never steal it) until shutdown cancels it.
	release, err := deploy.AcquireDeployLock("api", "rollback")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	var invocations sync.Map
	prep := &prepareRecorder{dir: fakeSourceDir}
	rb := &CliRebuild{poll: 5 * time.Millisecond, run: recordingRunner(&invocations), prepare: prep.preparer()}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rb.Rebuild(ctx, lockTestJob()) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !IsShutdownErr(err) {
			t.Fatalf("waiting job at shutdown returned %v, want a shutdown error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rebuild did not abort at shutdown")
	}
	if _, ran := invocations.Load("/srv/api"); ran {
		t.Fatal("job started a rebuild despite shutdown")
	}
	if n := prep.cleanupCount(); n != 1 {
		t.Fatalf("isolated source cleaned %d times at shutdown, want 1 (nothing was running)", n)
	}
}

func TestCliRebuild_ShutdownWithSubprocessRunningKeepsSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	prep := &prepareRecorder{dir: fakeSourceDir}
	rb := &CliRebuild{
		poll:    5 * time.Millisecond,
		prepare: prep.preparer(),
		run: func(ctx context.Context, job *Job, dir string, args ...string) ([]string, error) {
			// The rebuild subprocess is "running" when the server shuts down.
			return nil, phelixerr.Wrap(phelixerr.CodeUnavailable,
				"webhook: server shutdown; rebuild left running to completion",
				errors.Join(ErrShutdown, ErrShutdownSubprocess))
		},
	}

	if err := rb.Rebuild(context.Background(), lockTestJob()); !IsShutdownErr(err) {
		t.Fatalf("rebuild = %v, want a shutdown error", err)
	}
	if n := prep.cleanupCount(); n != 0 {
		t.Fatalf("isolated source was removed under a running rebuild (cleaned %d times, want 0 — the startup sweep owns it)", n)
	}
}

func TestCliRebuild_GitSyncFailureNeverRunsRebuild(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	requireGit(t)

	repo := newGitRepo(t) // app dir is a valid repo with remote and commits

	var invocations sync.Map
	rb := &CliRebuild{
		poll:    5 * time.Millisecond,
		run:     recordingRunner(&invocations),
		prepare: NewGitSourcePreparer(filepath.Join(t.TempDir(), "wt")),
	}
	job := &Job{
		AppID: "app-1", AppName: "api",
		Directory: repo.appDir, Branch: "main",
		CommitSHA:  "ffffffffffffffffffffffffffffffffffffffff", // never pushed
		DeliveryID: "d-gitsync-1",
	}

	err := rb.Rebuild(context.Background(), job)
	if err == nil {
		t.Fatal("expected a git sync failure")
	}
	if !phelixerr.IsCode(err, phelixerr.CodeGitSyncFailed) {
		t.Fatalf("error code = %v, want GIT_SYNC_FAILED (%v)", phelixerr.CodeOf(err), err)
	}
	if _, ran := invocations.Load(repo.appDir); ran {
		t.Fatal("rebuild ran despite a git synchronization failure")
	}
}

// The queued execution must reach the rebuild invocation through the deploy
// lock boundary: with the lock held by a manual operation, the queued job
// waits; once free, the job invokes the rebuild command against the isolated
// exact-commit source.
func TestQueue_JobReachesRebuildThroughDeployLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var invocations sync.Map
	prep := &prepareRecorder{dir: fakeSourceDir}
	rb := &CliRebuild{poll: 5 * time.Millisecond, run: recordingRunner(&invocations), prepare: prep.preparer()}
	queue := NewQueue(QueueOptions{Rebuild: rb.Rebuild})
	t.Cleanup(func() { queue.Close(2 * time.Second) })

	release, err := deploy.AcquireDeployLock("api", "rebuild")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- queue.Enqueue(lockTestJob())
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue blocked on the deploy lock")
	}

	// The job is queued but must not start the rebuild while the lock is held.
	time.Sleep(150 * time.Millisecond)
	if _, ran := invocations.Load("/srv/api"); ran {
		t.Fatal("queued job bypassed the deploy lock")
	}

	release()
	deadline := time.After(2 * time.Second)
	for {
		if _, ran := invocations.Load("/srv/api"); ran {
			return
		}
		select {
		case <-deadline:
			t.Fatal("queued job never reached the rebuild after the lock was released")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------------------
// Delivery ledger
// ---------------------------------------------------------------------------

func TestDeliveryLedger(t *testing.T) {
	t.Run("first record then seen", func(t *testing.T) {
		l := NewDeliveryLedger(filepath.Join(t.TempDir(), "deliveries.json"), 16)
		if err := l.Initialize(); err != nil {
			t.Fatal(err)
		}
		seen, err := l.SeenOrRecord("api", "d-1", "main", testCommit, ProviderGitHub)
		if err != nil || seen {
			t.Fatalf("first: seen=%v err=%v", seen, err)
		}
		seen, err = l.SeenOrRecord("api", "d-1", "main", testCommit, ProviderGitHub)
		if err != nil || !seen {
			t.Fatalf("second: seen=%v err=%v", seen, err)
		}
		// Same delivery id for a different app is a different delivery.
		seen, err = l.SeenOrRecord("web", "d-1", "main", testCommit, ProviderGitHub)
		if err != nil || seen {
			t.Fatalf("other app: seen=%v err=%v", seen, err)
		}
	})

	t.Run("not initialized fails closed", func(t *testing.T) {
		l := NewDeliveryLedger(filepath.Join(t.TempDir(), "deliveries.json"), 16)
		if _, err := l.SeenOrRecord("api", "d-1", "main", testCommit, ProviderGitHub); err == nil {
			t.Fatal("expected an error from an uninitialized ledger")
		}
	})

	t.Run("remove undoes a record", func(t *testing.T) {
		l := NewDeliveryLedger(filepath.Join(t.TempDir(), "deliveries.json"), 16)
		if err := l.Initialize(); err != nil {
			t.Fatal(err)
		}
		if seen, _ := l.SeenOrRecord("api", "d-1", "main", testCommit, ProviderGitHub); seen {
			t.Fatal("unexpected seen on first record")
		}
		if err := l.Remove("api", "d-1"); err != nil {
			t.Fatal(err)
		}
		if seen, _ := l.SeenOrRecord("api", "d-1", "main", testCommit, ProviderGitHub); seen {
			t.Fatal("delivery still seen after Remove")
		}
	})

	t.Run("concurrent records produce one winner", func(t *testing.T) {
		l := NewDeliveryLedger(filepath.Join(t.TempDir(), "deliveries.json"), 64)
		if err := l.Initialize(); err != nil {
			t.Fatal(err)
		}
		const n = 16
		var seenCount int32
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				seen, err := l.SeenOrRecord("api", "d-conc", "main", testCommit, ProviderGitHub)
				if err != nil {
					t.Errorf("record: %v", err)
					return
				}
				if seen {
					atomic.AddInt32(&seenCount, 1)
				}
			}()
		}
		wg.Wait()
		if got := atomic.LoadInt32(&seenCount); got != n-1 {
			t.Fatalf("%d goroutines saw a duplicate, want %d", got, n-1)
		}
	})

	t.Run("corrupt file fails initialization", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "deliveries.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		l := NewDeliveryLedger(path, 16)
		if err := l.Initialize(); err == nil {
			t.Fatal("expected corrupt ledger to fail initialization")
		}
	})

	t.Run("capacity evicts oldest", func(t *testing.T) {
		l := NewDeliveryLedger(filepath.Join(t.TempDir(), "deliveries.json"), 2)
		if err := l.Initialize(); err != nil {
			t.Fatal(err)
		}
		l.SeenOrRecord("api", "d-1", "main", testCommit, ProviderGitHub)
		l.SeenOrRecord("api", "d-2", "main", testCommit, ProviderGitHub)
		l.SeenOrRecord("api", "d-3", "main", testCommit, ProviderGitHub)
		if got := l.Len(); got != 2 {
			t.Fatalf("len = %d, want 2", got)
		}
		if seen, _ := l.SeenOrRecord("api", "d-1", "main", testCommit, ProviderGitHub); seen {
			t.Fatal("oldest delivery should have been evicted")
		}
	})
}

// ---------------------------------------------------------------------------
// Payload helpers
// ---------------------------------------------------------------------------

func TestBranchFromRef(t *testing.T) {
	cases := map[string]string{
		"refs/heads/main":        "main",
		"main":                   "main",
		"refs/heads/release/2.x": "release/2.x",
		"release/2.x":            "release/2.x",
		"refs/tags/v1":           "refs/tags/v1",
		"":                       "",
	}
	for in, want := range cases {
		if got := BranchFromRef(in); got != want {
			t.Fatalf("BranchFromRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsBranchDeletion(t *testing.T) {
	if !IsBranchDeletion(&PushPayload{Deleted: true, After: testCommit}) {
		t.Fatal("deleted=true must be a deletion")
	}
	if !IsBranchDeletion(&PushPayload{After: strings.Repeat("0", 40)}) {
		t.Fatal("all-zero after SHA must be a deletion")
	}
	if IsBranchDeletion(&PushPayload{After: testCommit}) {
		t.Fatal("a normal push is not a deletion")
	}
}

func TestParsePushPayload(t *testing.T) {
	p, err := ParsePushPayload([]byte(`{"ref":"refs/heads/main","after":"abc123"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Ref != "refs/heads/main" || p.After != "abc123" {
		t.Fatalf("payload = %+v", p)
	}
	if _, err := ParsePushPayload([]byte("nope")); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestIgnoredReason(t *testing.T) {
	push := &PushPayload{Ref: "refs/heads/main", After: testCommit}
	if got := ignoredReason("push", push, "main", "main"); got != "" {
		t.Fatalf("matching push must not be ignored, got %q", got)
	}
	if got := ignoredReason("push", push, "dev", "main"); got != "branch does not match configured branch" {
		t.Fatalf("mismatch reason = %q", got)
	}
	if got := ignoredReason("ping", push, "main", "main"); got != "event ignored" {
		t.Fatalf("ping reason = %q", got)
	}
	deleted := &PushPayload{Ref: "refs/heads/main", After: strings.Repeat("0", 40)}
	if got := ignoredReason("push", deleted, "main", "main"); got != "branch deleted; nothing to rebuild" {
		t.Fatalf("deletion reason = %q", got)
	}
}

// ---------------------------------------------------------------------------
// App resolution from real phelix.yaml files
// ---------------------------------------------------------------------------

func writeProject(t *testing.T, dir, yaml string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, project.FileName), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveAppDir(t *testing.T) {
	t.Run("enabled webhook config loads", func(t *testing.T) {
		dir := writeProject(t, t.TempDir(), "name: api\nwebhook:\n  enabled: true\n  branch: main\n  secret_env: PHELIX_WEBHOOK_SECRET\n")
		resolved, err := resolveAppDir(AppInfo{ID: "app-1", Name: "api", Directory: dir})
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Config == nil || !resolved.Config.Enabled || resolved.Config.Branch != "main" || resolved.Config.SecretEnv != "PHELIX_WEBHOOK_SECRET" {
			t.Fatalf("config = %+v", resolved.Config)
		}
	})

	t.Run("missing phelix.yaml means disabled", func(t *testing.T) {
		resolved, err := resolveAppDir(AppInfo{ID: "app-1", Name: "api", Directory: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Config != nil {
			t.Fatalf("expected nil config, got %+v", resolved.Config)
		}
	})

	t.Run("invalid phelix.yaml fails closed", func(t *testing.T) {
		dir := writeProject(t, t.TempDir(), "webhook:\n  enabled: true\n  branch: main\n  secret_env: PHELIX_WEBHOOK_SECRET\nhealth:\n  endpoints:\n    - name: x\n      path: bad\n")
		_, err := resolveAppDir(AppInfo{ID: "app-1", Name: "api", Directory: dir})
		if err == nil {
			t.Fatal("expected an error for an invalid phelix.yaml")
		}
		if !phelixerr.IsCode(err, phelixerr.CodeConfiguration) {
			t.Fatalf("error code = %v", phelixerr.CodeOf(err))
		}
	})
}

func TestValidateSecrets(t *testing.T) {
	enabled := func(dir string) AppInfo { return AppInfo{ID: "app-1", Name: "api", Directory: dir} }
	lookup := func(name string) (string, bool) {
		if name == "PHELIX_WEBHOOK_SECRET" {
			return testSecret, true
		}
		return "", false
	}

	t.Run("enabled with secret set passes", func(t *testing.T) {
		dir := writeProject(t, t.TempDir(), "name: api\nwebhook:\n  enabled: true\n  branch: main\n  secret_env: PHELIX_WEBHOOK_SECRET\n")
		if err := validateSecrets([]AppInfo{enabled(dir)}, lookup); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("enabled with missing secret fails clearly", func(t *testing.T) {
		dir := writeProject(t, t.TempDir(), "name: api\nwebhook:\n  enabled: true\n  branch: main\n  secret_env: MISSING_SECRET\n")
		err := validateSecrets([]AppInfo{enabled(dir)}, lookup)
		if err == nil {
			t.Fatal("expected a startup failure for the missing secret")
		}
		msg := err.Error()
		if !strings.Contains(msg, "api") || !strings.Contains(msg, "MISSING_SECRET") {
			t.Fatalf("error must name the app and the env var, got: %s", msg)
		}
	})

	t.Run("disabled app without secret passes", func(t *testing.T) {
		dir := writeProject(t, t.TempDir(), "name: api\nwebhook:\n  enabled: false\n  branch: main\n  secret_env: MISSING_SECRET\n")
		if err := validateSecrets([]AppInfo{enabled(dir)}, lookup); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("no webhook section passes", func(t *testing.T) {
		dir := writeProject(t, t.TempDir(), "name: api\n")
		if err := validateSecrets([]AppInfo{enabled(dir)}, lookup); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("app with invalid yaml is skipped, not fatal", func(t *testing.T) {
		dir := writeProject(t, t.TempDir(), "webhook:\n  enabled: true\n  branch: main\n  secret_env: MISSING_SECRET\nhealth:\n  endpoints:\n    - name: x\n      path: bad\n")
		if err := validateSecrets([]AppInfo{enabled(dir)}, lookup); err != nil {
			t.Fatalf("invalid yaml of a foreign app must not block startup: %v", err)
		}
	})
}
