package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/logs"
)

func resetAccessSink(t *testing.T) string {
	t.Helper()
	logs.CloseAccessLog()
	home := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", filepath.Join(home, ".phelix"))
	t.Cleanup(logs.CloseAccessLog)
	return home
}

func readAccessRecords(t *testing.T, home string) []logs.LogRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".phelix", "logs", "access.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []logs.LogRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var rec logs.LogRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("access log line is not a record: %v (%q)", err, line)
		}
		out = append(out, rec)
	}
	return out
}

func newTestProxy(t *testing.T, handler http.Handler, mode string) *Proxy {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	p := New("orders", 0, Target{Host: strings.TrimPrefix(backend.URL, "http://"), Label: "blue"})
	p.AccessMode = mode
	return p
}

func TestSampledAccess(t *testing.T) {
	cases := []struct {
		name   string
		mode   string
		status int
		lat    time.Duration
		p95    time.Duration
		want   bool
	}{
		{"default samples 5xx", "", 502, 0, 0, true},
		{"default samples 4xx", "", 404, 0, 0, true},
		{"default skips fast 200", "", 200, time.Millisecond, 0, false},
		{"default samples slow 200", "", 200, 2 * time.Second, time.Second, true},
		{"p95 zero never samples 200", "", 200, time.Hour, 0, false},
		{"all samples fast 200", AccessLogAll, 200, time.Millisecond, 0, true},
		{"off drops 5xx", AccessLogOff, 502, 0, 0, false},
	}
	for _, c := range cases {
		if got := sampledAccess(c.mode, c.status, c.lat, c.p95); got != c.want {
			t.Errorf("%s: sampled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAccessMiddleware_RecordsServerError(t *testing.T) {
	home := resetAccessSink(t)
	p := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}), "")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/things?token=secret", nil)
	req.Header.Set("Traceparent", "00-trace-span-01")
	(&accessMiddleware{proxy: p, next: p.rp}).ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	requestID := w.Header().Get("X-Request-Id")
	if requestID == "" {
		t.Fatalf("X-Request-Id must be echoed to the client")
	}

	recs := readAccessRecords(t, home)
	if len(recs) != 1 {
		t.Fatalf("got %d access records, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Source != logs.SourceProxy || rec.App != "orders" || rec.Level != logs.LevelError {
		t.Errorf("record header wrong: %+v", rec)
	}
	if rec.RequestID != requestID {
		t.Errorf("request id mismatch: record %q header %q", rec.RequestID, requestID)
	}
	if rec.Msg != "GET /api/things 500" {
		t.Errorf("msg = %q, want GET /api/things 500", rec.Msg)
	}
	if rec.Fields["path"] != "/api/things" {
		t.Errorf("path = %q, query string must be stripped", rec.Fields["path"])
	}
	for k, v := range rec.Fields {
		if strings.Contains(v, "secret") || strings.Contains(v, "token=") {
			t.Errorf("query string leaked into field %q: %q", k, v)
		}
	}
	if rec.Fields["status"] != "500" || rec.Fields["bytes"] != "4" {
		t.Errorf("status/bytes fields: %v", rec.Fields)
	}
	if rec.Fields["traceparent"] != "00-trace-span-01" {
		t.Errorf("traceparent field missing: %v", rec.Fields)
	}
	if rec.Fields["backend_label"] != "blue" {
		t.Errorf("backend label = %q, want blue", rec.Fields["backend_label"])
	}
}

func TestAccessMiddleware_SkipsSampledOK(t *testing.T) {
	home := resetAccessSink(t)
	p := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), "")

	w := httptest.NewRecorder()
	(&accessMiddleware{proxy: p, next: p.rp}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Header().Get("X-Request-Id") == "" {
		t.Errorf("request id must be set even when unsampled")
	}
	if recs := readAccessRecords(t, home); len(recs) != 0 {
		t.Errorf("fast 200 must not be sampled, got %d records", len(recs))
	}
}

func TestAccessMiddleware_AllAndOffModes(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	home := resetAccessSink(t)
	p := newTestProxy(t, handler, AccessLogAll)
	(&accessMiddleware{proxy: p, next: p.rp}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if recs := readAccessRecords(t, home); len(recs) != 1 {
		t.Errorf("all mode must record a 200, got %d", len(recs))
	}

	home = resetAccessSink(t)
	p = newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}), AccessLogOff)
	(&accessMiddleware{proxy: p, next: p.rp}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if recs := readAccessRecords(t, home); len(recs) != 0 {
		t.Errorf("off mode must record nothing, got %d", len(recs))
	}
}

func TestCollectAccessLogsRoundTrip(t *testing.T) {
	resetAccessSink(t)

	now := time.Now().UTC().Format(logs.RFC3339Milli)
	logs.AppendAccessRecord(logs.LogRecord{
		TS: now, Level: logs.LevelWarning, Source: logs.SourceProxy,
		App: "orders", AppID: "apps_1", RequestID: "req_42",
		Msg:    "GET /slow 404",
		Fields: map[string]string{"status": "404", "path": "/slow"},
	})

	entries, err := logs.CollectAccessLogs("srv-9")
	if err != nil {
		t.Fatalf("CollectAccessLogs returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.ServerID != "srv-9" || e.AppID != "apps_1" || e.App != "orders" || e.RequestID != "req_42" {
		t.Errorf("entry identity: %+v", e)
	}
	if e.Level != logs.LevelWarning || e.Fields["status"] != "404" {
		t.Errorf("entry content: %+v", e)
	}
}
