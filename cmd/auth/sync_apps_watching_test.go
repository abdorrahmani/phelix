package auth

// sync_apps_watching_test.go covers the watching filter of the REST app-list
// upload: apps with Watching=false must not be registered on the backend.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/config"
	"github.com/abdorrahmani/phelix/internal/app"
)

// appListStub serves a fixed app list; SendAppsToServer only enumerates apps
// through ListApplications, so no real state file is touched.
type appListStub struct {
	app.AppManagerInterface
	items []app.AppListItem
}

func (s *appListStub) ListApplications() []app.AppListItem { return s.items }
func (s *appListStub) LoadState() error                    { return nil }

// recordingTransport captures the outgoing POST instead of dialing the
// configured backend, which the test cannot reach (and must not).
type recordingTransport struct {
	mu     sync.Mutex
	bodies []string
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.bodies = append(t.bodies, string(body))
	t.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Header:     make(http.Header),
	}, nil
}

func (t *recordingTransport) lastBody() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.bodies) == 0 {
		return ""
	}
	return t.bodies[len(t.bodies)-1]
}

func TestSendAppsToServer_UploadsWatchedAppsOnly(t *testing.T) {
	if err := config.Load(); err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	isolatedHome(t)
	if err := storeSessionAt(&Session{
		SessionID: "sess-1",
		Token:     "synthetic-token",
		ExpiresAt: time.Now().Add(time.Hour),
	}, getSessionFilePath()); err != nil {
		t.Fatalf("storeSessionAt: %v", err)
	}

	origMgr := app.Manager
	app.Manager = &appListStub{items: []app.AppListItem{
		{ID: "a", Name: "watched", Status: "stopped", Watching: true},
		{ID: "b", Name: "unwatched", Status: "stopped", Watching: false},
	}}
	t.Cleanup(func() { app.Manager = origMgr })

	transport := &recordingTransport{}
	origTransport := http.DefaultClient.Transport
	http.DefaultClient.Transport = transport
	t.Cleanup(func() { http.DefaultClient.Transport = origTransport })

	if err := SendAppsToServer(); err != nil {
		t.Fatalf("SendAppsToServer: %v", err)
	}

	body := transport.lastBody()
	if body == "" {
		t.Fatal("no upload was recorded")
	}
	var payload struct {
		Apps []struct {
			Name string `json:"name"`
		} `json:"apps"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("upload body not parseable: %v\nbody: %s", err, body)
	}
	if len(payload.Apps) != 1 || payload.Apps[0].Name != "watched" {
		t.Fatalf("uploaded apps = %+v, want exactly [watched]", payload.Apps)
	}
}
