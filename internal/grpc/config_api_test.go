package grpc

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	phelixconfig "github.com/abdorrahmani/phelix/config"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/server"
)

func setupConfigTransport(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	setupTestSession(t)
	if err := phelixconfig.Load(); err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(handler)
	beforeBase, beforeClient := appConfigAPIBaseURL, appConfigHTTPClient
	appConfigAPIBaseURL, appConfigHTTPClient = backend.URL, nil
	t.Cleanup(func() { appConfigAPIBaseURL, appConfigHTTPClient = beforeBase, beforeClient; backend.Close() })
	return backend
}

func TestAppConfigTransportAuthenticatesAndEscapesRequests(t *testing.T) {
	id := "request&other=private%value+space /?"
	task := AppConfigTask{RequestID: id, Command: AppConfigCommandSave, AppName: "demo", Content: "name: demo\n", ExpectedRevision: "revision"}
	result := AppConfigResult{RequestID: id, Status: "success", Valid: true, Content: task.Content}
	received := make(chan AppConfigResult, 1)
	setupConfigTransport(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("X-Session-ID") != "test-session" || r.Header.Get("X-Server-ID") != server.GetServerID() {
			t.Error("missing agent authentication identity")
		}
		switch r.URL.Path {
		case "/phelix/agent/config/pending":
			if r.URL.Query().Get("request_id") != id || len(r.URL.Query()) != 1 {
				t.Errorf("request id escaped incorrectly: %s", r.URL.RawQuery)
			}
			json.NewEncoder(w).Encode(task)
		case "/phelix/agent/config/result":
			var got AppConfigResult
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Error(err)
			}
			received <- got
		default:
			http.NotFound(w, r)
		}
	})
	got, err := TakeAppConfigTask(id)
	if err != nil || got.Content != task.Content || got.RequestID != id {
		t.Fatalf("task: %+v %v", got, err)
	}
	if err := SubmitAppConfigResult(result); err != nil {
		t.Fatal(err)
	}
	if got := <-received; got.Content != result.Content || !got.Valid {
		t.Fatalf("result: %+v", got)
	}
}

func TestAppConfigTransportRejectsRedirectsAndLargeResponses(t *testing.T) {
	for _, kind := range []string{"redirect", "oversized", "malformed", "denied"} {
		t.Run(kind, func(t *testing.T) {
			setupConfigTransport(t, func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "redirect":
					http.Redirect(w, r, "/different", http.StatusFound)
				case "oversized":
					w.Write([]byte(`{"content":"` + strings.Repeat("x", 4<<20) + `"}`))
				case "malformed":
					w.Write([]byte("{"))
				case "denied":
					http.Error(w, "private configuration contents", http.StatusForbidden)
				}
			})
			_, err := TakeAppConfigTask("request")
			if err == nil {
				t.Fatal("unsafe response accepted")
			}
			if strings.Contains(err.Error(), "private configuration contents") {
				t.Fatal("response body leaked in error")
			}
		})
	}
}

type failingConfigTransport struct{}

func (failingConfigTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, errors.New("connection unavailable")
}

func TestAppConfigTransportNetworkFailure(t *testing.T) {
	setupConfigTransport(t, func(w http.ResponseWriter, r *http.Request) {})
	appConfigHTTPClient = &http.Client{Transport: failingConfigTransport{}}
	if _, err := TakeAppConfigTask("request"); err == nil {
		t.Fatal("network failure ignored")
	}
	if err := SubmitAppConfigResult(AppConfigResult{RequestID: "request", Status: "success"}); err == nil {
		t.Fatal("result network failure ignored")
	}
}

func TestAppConfigCommandsBoundConcurrency(t *testing.T) {
	original := getAppConfigHandler()
	t.Cleanup(func() { SetAppConfigHandler(original) })
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	var calls atomic.Int32
	SetAppConfigHandler(func(req *pb.MonitorCommandRequest) { calls.Add(1); started <- struct{}{}; <-release })
	client := NewClient()
	for i := 0; i < 8; i++ {
		client.handleAppConfigCommand(&pb.MonitorCommandRequest{Type: AppConfigCommandRead, RequestId: "request"})
	}
	for i := 0; i < 8; i++ {
		<-started
	}
	client.handleAppConfigCommand(&pb.MonitorCommandRequest{Type: AppConfigCommandRead, RequestId: "overflow"})
	if calls.Load() != 8 {
		t.Fatalf("concurrent handlers exceeded bound: %d", calls.Load())
	}
	close(release)
	pendingAppConfigWG.Wait()
}

func TestAppConfigProtocolRejectsLegacyCommands(t *testing.T) {
	for _, command := range []string{"config_read", "config_save", "config_create", "config_validate"} {
		if IsAppConfigCommand(command) {
			t.Fatalf("legacy command %s bypasses scoped configuration protocol", command)
		}
		if !IsAppConfigCommand(command + "_v2") {
			t.Fatalf("versioned command %s rejected", command)
		}
	}
}
