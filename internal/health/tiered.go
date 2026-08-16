package health

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Tier identifies which deploy-time health-check strategy is in use.
type Tier int

const (
	// TierUnknown is the zero value and means "not yet selected".
	TierUnknown Tier = 0

	// Tier1HTTPPath: the user configured an explicit health endpoint path.
	// A 2xx response on that path is required.
	Tier1HTTPPath Tier = 1

	// Tier2HTTPAny: no path configured, but the app serves HTTP on its port.
	// ANY valid HTTP response (200, 404, 500, ...) counts as "alive"; only
	// connection-refused / timeout counts as unhealthy. The goal is to prove
	// the server is up and answering, NOT that a specific route works.
	Tier2HTTPAny Tier = 2

	// Tier3TCP: the app is not (known to be) an HTTP server, or the user opted
	// into a lighter mode. We only confirm the port accepts TCP connections.
	Tier3TCP Tier = 3

	// Tier3None: the app is a non-HTTP worker/daemon. We only confirm the
	// process PID is alive; no network probe at all.
	Tier3None Tier = 4
)

// String returns a human label for a tier, used in warnings and status output.
func (t Tier) String() string {
	switch t {
	case Tier1HTTPPath:
		return "Tier 1 (explicit health endpoint, 2xx required)"
	case Tier2HTTPAny:
		return "Tier 2 (HTTP probe, any response = alive)"
	case Tier3TCP:
		return "Tier 3 (TCP port check only)"
	case Tier3None:
		return "Tier 3 (process PID check only)"
	default:
		return "unknown"
	}
}

// Default deploy-time health parameters (see DeployTierConfig for per-app override).
const (
	DefaultTierInterval = 1 * time.Second
	DefaultTierRetries  = 5
	DefaultTierTimeout  = 30 * time.Second
	DefaultProbeTimeout = 2 * time.Second // per-probe HTTP/TCP dial timeout
)

// HTTPProber is the contract for a single HTTP probe. It returns the response
// (whose status code the caller inspects) or an error (connection refused,
// timeout, etc.). It's a function type so tests can inject fake servers.
type HTTPProber func(ctx context.Context, url string) (*http.Response, error)

// defaultHTTPProbe uses a short-timeout http.Client to GET the URL.
func defaultHTTPProbe(ctx context.Context, url string) (*http.Response, error) {
	client := &http.Client{Timeout: DefaultProbeTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// TCPDialer is the contract for a single TCP probe. Returns nil if the port
// accepts a connection within the timeout.
type TCPDialer func(addr string, timeout time.Duration) error

// defaultTCPDial wraps net.DialTimeout.
func defaultTCPDial(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// PidChecker returns true if the process with the given PID is still alive.
type PidChecker func(pid int) bool

// defaultPidAlive sends signal 0 to the process; an error means it's gone.
func defaultPidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 is the standard "existence" check on POSIX. os.Signal(nil)
	// delivers no actual signal but still validates the PID.
	return p.Signal(os.Signal(nil)) == nil
}

// tierResolver bundles the pluggable probes so tests can inject fakes. A nil
// field falls back to the real implementation.
type tierResolver struct {
	httpProbe HTTPProber
	tcpDial   TCPDialer
	pidAlive  PidChecker
}

func (r *tierResolver) probe() HTTPProber {
	if r.httpProbe != nil {
		return r.httpProbe
	}
	return defaultHTTPProbe
}

func (r *tierResolver) dial() TCPDialer {
	if r.tcpDial != nil {
		return r.tcpDial
	}
	return defaultTCPDial
}

func (r *tierResolver) alive() PidChecker {
	if r.pidAlive != nil {
		return r.pidAlive
	}
	return defaultPidAlive
}

// SelectTier decides which tier applies for the given app at deploy time.
//
// Selection order (per the project spec):
//  1. --mode none            -> Tier3None
//  2. --mode tcp-only        -> Tier3TCP
//  3. explicit --path set    -> Tier1HTTPPath (mode auto/http both honor this)
//  4. else probe HTTP "/":   if the port answers with ANY HTTP response -> Tier2HTTPAny
//  5. no HTTP response       -> Tier3TCP (port still accepts TCP, just not HTTP)
//
// The HTTP probe in step 4 is what lets us distinguish a real HTTP server from
// a raw TCP worker. We never return TierUnknown from this function.
func SelectTier(cfg *DeployTierConfig, host string, resolver *tierResolver) Tier {
	if resolver == nil {
		resolver = &tierResolver{}
	}
	mode := TierModeAuto
	if cfg != nil && cfg.Mode != "" {
		mode = cfg.Mode
	}

	// (1) / (2) explicit light modes
	switch mode {
	case TierModeNone:
		return Tier3None
	case TierModeTCPOnly:
		return Tier3TCP
	}

	// (3) explicit path selects Tier 1 for both auto and http modes.
	if cfg != nil && cfg.Path != "" {
		return Tier1HTTPPath
	}
	if mode == TierModeHTTP {
		// User forced http mode but supplied no path: we can't probe a specific
		// endpoint, so fall back to Tier 2 (any HTTP response) rather than
		// guessing a path. This is the safest interpretation.
		return Tier2HTTPAny
	}

	// (4)/(5) auto-detect: probe the root path once.
	ctx, cancel := context.WithTimeout(context.Background(), DefaultProbeTimeout)
	defer cancel()
	resp, err := resolver.probe()(ctx, "http://"+host+"/")
	if err != nil {
		// Connection refused / timeout / non-HTTP responder -> Tier 3 TCP.
		return Tier3TCP
	}
	_ = resp.Body.Close()
	// Any valid HTTP status (including 404/500) means an HTTP server is up.
	return Tier2HTTPAny
}

// resolvedConfig normalises a (possibly nil) DeployTierConfig to concrete
// values, applying the documented defaults.
func resolvedConfig(cfg *DeployTierConfig) (interval time.Duration, retries int, timeout time.Duration) {
	interval = DefaultTierInterval
	retries = DefaultTierRetries
	timeout = DefaultTierTimeout

	if cfg == nil {
		return
	}
	if d, err := time.ParseDuration(cfg.Interval); err == nil && d > 0 {
		interval = d
	}
	if cfg.Retries > 0 {
		retries = cfg.Retries
	}
	if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
		timeout = d
	}
	return
}

// Check performs ONE probe appropriate to the tier and returns true if the
// target is considered alive at this instant. This is the single-shot primitive
// that WaitForHealthy polls.
//
// Why Tier 2 (Tier2HTTPAny) deliberately does NOT require a 2xx status:
// We are only trying to prove the process is up and the network stack is
// answering — i.e. that routing traffic to it won't immediately fail. Many
// legitimate apps return 404 at "/" (no root handler) or even 500 while a
// dependency warms up. If we required 2xx here, every such app would be
// falsely declared unhealthy during a deploy, aborting the cut-over and
// defeating the entire purpose of zero-downtime. Requiring a semantic success
// status is reserved for Tier 1, where the user has explicitly opted into a
// health endpoint and therefore vouches that a 2xx there is meaningful.
func Check(ctx context.Context, tier Tier, cfg *DeployTierConfig, host string, pid int, resolver *tierResolver) bool {
	if resolver == nil {
		resolver = &tierResolver{}
	}

	switch tier {
	case Tier1HTTPPath:
		// User-supplied path; require 2xx.
		path := "/"
		if cfg != nil && cfg.Path != "" {
			path = cfg.Path
		}
		pctx, cancel := context.WithTimeout(ctx, DefaultProbeTimeout)
		defer cancel()
		resp, err := resolver.probe()(pctx, "http://"+host+path)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300

	case Tier2HTTPAny:
		// Any HTTP response = alive. Only connection/timeout errors fail.
		pctx, cancel := context.WithTimeout(ctx, DefaultProbeTimeout)
		defer cancel()
		resp, err := resolver.probe()(pctx, "http://"+host+"/")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return true

	case Tier3TCP:
		return resolver.dial()(host, DefaultProbeTimeout) == nil

	case Tier3None:
		return resolver.alive()(pid)
	}
	return false
}

// WaitForHealthy polls Check until it reports `retries` consecutive successes
// or the overall `timeout` elapses. Returns nil if healthy, an error describing
// why (timeout / process died) otherwise. The interval, retries and timeout
// come from cfg (falling back to defaults). resolver may be nil.
func WaitForHealthy(ctx context.Context, tier Tier, cfg *DeployTierConfig, host string, pid int, resolver *tierResolver) error {
	interval, retries, timeout := resolvedConfig(cfg)

	// The overall deadline is the smaller of ctx's deadline and our timeout.
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	consecutive := 0
	// Do a first check immediately rather than waiting one interval.
	for first := true; ; first = false {
		if !first {
			select {
			case <-ctx.Done():
				// Preserve the context cause (cancel / deadline) so callers can
				// errors.Is against context.Canceled / context.DeadlineExceeded.
				return ctx.Err()
			case <-time.After(time.Until(deadline)):
				return phelixerr.Newf(phelixerr.CodeHealthCheckFailed, "health check timed out after %s (tier %s, %d/%d consecutive successes)",
					timeout, tier, consecutive, retries)
			case <-ticker.C:
			}
		}
		if time.Now().After(deadline) {
			return phelixerr.Newf(phelixerr.CodeHealthCheckFailed, "health check timed out after %s (tier %s, %d/%d consecutive successes)",
				timeout, tier, consecutive, retries)
		}

		if Check(ctx, tier, cfg, host, pid, resolver) {
			consecutive++
			if consecutive >= retries {
				return nil
			}
		} else {
			consecutive = 0
		}
	}
}

// MustParseHostPort splits a "host:port" string into its parts, defaulting the
// host to localhost when only a port is given. Small helper used by the deploy
// package; lives here next to the other tier helpers.
func MustParseHostPort(addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		// Maybe it's just a port.
		if p, err2 := strconv.Atoi(addr); err2 == nil {
			return "localhost", p, nil
		}
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	if host == "" {
		host = "localhost"
	}
	return host, port, nil
}
