package cmd

import (
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/logs"
)

// startPprofIfEnabled starts a net/http/pprof server when PHELIX_PPROF is set,
// used only for local resource profiling of the daemon. It is off by default
// and never registers on the process-wide DefaultServeMux, so nothing is
// exposed unless an operator explicitly opts in.
//
// PHELIX_PPROF holds a bind address, e.g. "127.0.0.1:6060" or ":6060". For
// safety a bare port (e.g. "6060") or an empty host binds to loopback only —
// pprof exposes goroutine dumps and can trigger CPU/heap work, so it must never
// be reachable off-box. Binding to a non-loopback host is honored but logged as
// a warning.
//
// Returns a shutdown func the caller defers; nil when profiling is disabled.
func startPprofIfEnabled() func() {
	addr := strings.TrimSpace(os.Getenv("PHELIX_PPROF"))
	if addr == "" {
		return nil
	}
	addr = normalizePprofAddr(addr)

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if host, _, err := net.SplitHostPort(addr); err == nil && !isLoopbackHost(host) {
		logs.Warning("monitor",
			"PHELIX_PPROF bound to non-loopback address %s; pprof exposes goroutine dumps and CPU/heap profiling — restrict to 127.0.0.1 unless you know what you're doing", addr)
	}

	go func() {
		logs.Info("monitor", "pprof profiling server listening on http://%s/debug/pprof/", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logs.Warning("monitor", "pprof server stopped: %v", err)
		}
	}()

	return func() { _ = srv.Close() }
}

// normalizePprofAddr accepts a bare port ("6060"), a ":port" (":6060"), or a
// full host:port and returns a host:port, defaulting the host to loopback when
// none was given.
func normalizePprofAddr(addr string) string {
	if !strings.Contains(addr, ":") {
		return "127.0.0.1:" + addr
	}
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}
