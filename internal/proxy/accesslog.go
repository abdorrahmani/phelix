package proxy

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/abdorrahmani/phelix/internal/logs"
)

// Access log modes configured per app in phelix.yaml (logging.access_log).
// An empty value means the default sampling: errors and slow requests only.
const (
	AccessLogAll = "all"
	AccessLogOff = "off"
)

// accessModeResolver resolves an app name to its configured access-log mode.
// It is injected by the app package at init because the proxy must stay
// decoupled from the app manager while the enroll call sites (the deploy
// engines) carry no app configuration.
var accessModeResolver func(appName string) string

// SetAccessModeResolver wires the per-app access-log mode lookup.
func SetAccessModeResolver(fn func(appName string) string) {
	accessModeResolver = fn
}

// resolveAccessMode returns the explicit mode when set, falling back to the
// injected lookup.
func resolveAccessMode(appName, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if accessModeResolver == nil {
		return ""
	}
	return accessModeResolver(appName)
}

// sampledAccess reports whether one request leaves an access record. The
// default mode logs every 4xx and 5xx (plus transport failures, which the
// error handler surfaces as 502) and every request slower than the app's
// current p95; "all" and "off" disable sampling in either direction.
func sampledAccess(mode string, status int, latency, p95 time.Duration) bool {
	switch mode {
	case AccessLogAll:
		return true
	case AccessLogOff:
		return false
	}
	if status >= 400 {
		return true
	}
	return p95 > 0 && latency > p95
}

// accessMiddleware is the outermost handler of every proxied request. It
// stamps a request id into the response, samples the request into the access
// log, and counts the bytes served so slow or failing requests can be
// identified one by one instead of as an aggregate.
type accessMiddleware struct {
	proxy *Proxy
	next  http.Handler
}

func (m *accessMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := logs.NewRequestID()
	r.Header.Set("X-Request-Id", requestID)
	w.Header().Set("X-Request-Id", requestID)

	rec := &accessRecorder{ResponseWriter: w, status: http.StatusOK}
	m.next.ServeHTTP(rec, r)

	m.proxy.recordAccess(r, rec, requestID, start, time.Since(start))
}

// recordAccess samples one completed request into the access log.
func (p *Proxy) recordAccess(r *http.Request, rec *accessRecorder, requestID string, start time.Time, latency time.Duration) {
	if p.AccessMode == AccessLogOff {
		return
	}
	if !sampledAccess(p.AccessMode, rec.status, latency, p.accessP95()) {
		return
	}

	level := logs.LevelInfo
	switch {
	case rec.status >= 500:
		level = logs.LevelError
	case rec.status >= 400:
		level = logs.LevelWarning
	}

	fields := map[string]string{
		"method":      r.Method,
		"path":        r.URL.Path,
		"status":      strconv.Itoa(rec.status),
		"bytes":       strconv.FormatInt(rec.bytes, 10),
		"duration_ms": strconv.FormatInt(latency.Milliseconds(), 10),
	}
	if host, ok := p.chosenBackend(requestID); ok {
		fields["backend"] = host
		if label := p.backendLabel(host); label != "" {
			fields["backend_label"] = label
		}
	}
	if tp := r.Header.Get("Traceparent"); tp != "" {
		fields["traceparent"] = tp
	}

	logs.AppendAccessRecord(logs.LogRecord{
		TS:        start.UTC().Format(logs.RFC3339Milli),
		Level:     level,
		Source:    logs.SourceProxy,
		App:       p.AppName,
		RequestID: requestID,
		Msg:       r.Method + " " + r.URL.Path + " " + strconv.Itoa(rec.status),
		Fields:    fields,
	})
}

// accessP95 returns the app-level p95 across all enrolled backends.
func (p *Proxy) accessP95() time.Duration {
	return Summarize(p.Stats()).P95
}

// chosenBackend pops the backend host the Director picked for a request id.
func (p *Proxy) chosenBackend(requestID string) (string, bool) {
	v, ok := p.chosen.LoadAndDelete(requestID)
	if !ok {
		return "", false
	}
	host, _ := v.(string)
	return host, host != ""
}

// backendLabel resolves a backend host to its routing label ("blue",
// "green", "replica-2", a canary slot).
func (p *Proxy) backendLabel(host string) string {
	for _, t := range p.loadState().targets {
		if t.Host == host {
			return t.Label
		}
	}
	return ""
}

// accessRecorder captures the status code and byte count of the response
// while passing every write through untouched.
type accessRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *accessRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *accessRecorder) Write(b []byte) (int, error) {
	r.bytes += int64(len(b))
	return r.ResponseWriter.Write(b)
}

func (r *accessRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *accessRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("proxy: response writer does not support hijacking")
}

// noteChosenBackend records which backend a request id was routed to, so the
// access middleware can attribute the record even when several backends are
// enrolled. Called from the counting transport, which sees the Director's
// choice.
func (p *Proxy) noteChosenBackend(requestID, host string) {
	if requestID == "" || host == "" {
		return
	}
	p.chosen.Store(requestID, host)
}
