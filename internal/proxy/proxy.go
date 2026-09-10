// Package proxy implements the zero-downtime reverse proxy used by Phelix's
// blue-green and rolling deploy flows.
//
// The public port the user chose (e.g. :8080) is bound exactly once and stays
// bound for the lifetime of the proxy daemon. Behind it, the proxy routes each
// incoming request to whichever internal instance (blue/green or one of N
// replicas) is currently "active". Switching the target is an atomic pointer
// swap that takes effect on the *next* request, so in-flight requests are
// never corrupted and there are no dropped connections.
package proxy

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Target describes one routable backend instance.
type Target struct {
	// Host is the "host:port" dial address of the backend, e.g. "127.0.0.1:9001".
	Host string
	// Label is an opaque, human-readable name for the instance (e.g. "blue").
	Label string
	// Weight is this backend's share of traffic in percent (1-100) when the
	// app is enrolled with more than one backend. Zero means "unweighted":
	// every unweighted backend in a set receives an equal share, which is the
	// pre-canary rolling behavior. Weights are hints, not guarantees — a
	// backend that fails its liveness probe receives nothing until it answers
	// again.
	Weight int
}

// LatencyBucketBounds are the upper bounds (inclusive) of the latency buckets
// reported in BackendStat.Buckets. Bucket i counts requests whose round-trip
// took at most LatencyBucketBounds[i]; requests slower than the last bound land
// in the last bucket. The boundaries mirror the Caddy histogram the health
// package already reports so both metric sources compare like for like.
var LatencyBucketBounds = [...]time.Duration{
	5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond,
	50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond,
	500 * time.Millisecond, time.Second, 2500 * time.Millisecond,
	5 * time.Second, 10 * time.Second,
}

// BackendStat is a snapshot of the request statistics one backend has served
// since the proxy started. Counters are cumulative; consumers that want
// windowed rates (the canary rollout verifier does) diff two snapshots.
type BackendStat struct {
	// Label is the target label at snapshot time ("" when the target is no
	// longer enrolled).
	Label string `json:"label,omitempty"`
	// Host identifies the backend: the Target.Host it was enrolled with.
	Host string `json:"host"`
	// Requests is the total number of requests routed to this backend,
	// including ones that failed to connect.
	Requests int64 `json:"requests"`
	// Errors is the number of those requests that answered 5xx or never got a
	// response at all (dial failure, timeout).
	Errors int64 `json:"errors"`
	// Buckets is the cumulative latency histogram: Buckets[i] is the number of
	// requests that completed within LatencyBucketBounds[i].
	Buckets []uint64 `json:"buckets,omitempty"`
}

// proxyState is the value stored in the atomic.Value. We always store a *value
// of this concrete type (never nil), which keeps the per-request Director read
// lock-free and safe.
type proxyState struct {
	// primary is the target new requests are routed to.
	primary Target
	// targets is the full set of healthy backends (used for rolling deploys and
	// for status reporting). primary is always a member of targets when set.
	targets []Target
}

// Proxy is the per-app reverse proxy. It owns one http.Server on PublicPort
// and one httputil.ReverseProxy whose Director reads the current target from
// an atomic.Value on every request.
type Proxy struct {
	// PublicPort is the externally exposed port the user asked for, e.g. 8080.
	// It never changes for the life of the proxy.
	PublicPort int

	// AppName is the app this proxy fronts (used in logs / status).
	AppName string

	state atomic.Value // *proxyState
	rp    *httputil.ReverseProxy

	server *http.Server

	// inFlight counts requests currently being proxied. It lets graceful
	// shutdown report "how many requests were still active at shutdown time".
	inFlight int64

	// wrrMu guards the smooth weighted round-robin counters below. It is held
	// only for a few map operations per request — never during I/O.
	wrrMu sync.Mutex
	// wrr holds one smooth-WRR current-weight counter per backend host. It is
	// reset whenever the backend set changes so stale counters cannot skew a
	// new set's distribution.
	wrr map[string]int64

	// statsMu guards backendStats. The critical section is a handful of
	// increments; the mutex keeps the per-request cost predictable without
	// atomics-per-field.
	statsMu sync.Mutex
	// backendStats accumulates per-backend request counters keyed by Target.Host.
	backendStats map[string]*backendCounters

	// liveness caches passive TCP probe results keyed by "host:port" so the
	// Director does not dial more than once per TTL window per backend.
	liveMu      sync.Mutex
	liveResults map[string]livenessResult

	// started guards against double Start.
	started atomic.Bool
	mu      sync.Mutex // serialises Start / SetTarget / Shutdown externally
}

// livenessResult records the outcome of the most recent reachability probe.
type livenessResult struct {
	ok bool
	at time.Time
}

const (
	livePassTTL = 500 * time.Millisecond // trust "reachable" briefly
	liveFailTTL = 200 * time.Millisecond // retry "unreachable" quickly
	liveDialTO  = 150 * time.Millisecond
)

// pickHealthyHost chooses which backend serves the next request when several
// are enrolled: smooth weighted round-robin over targets whose most recent TCP
// probe passed; backends without a fresh verdict are probed once and
// remembered. Weights come from the enrolled targets (unweighted = equal
// share), so a 95/5 stable/canary split is served as exactly 95%/5% over time
// with the canary requests spread evenly instead of bursting. When every
// target currently looks unreachable we degrade gracefully to the primary so
// behaviour matches the pre-failover contract instead of blackholing.
func (p *Proxy) pickHealthyHost(st *proxyState) string {
	n := len(st.targets)
	if n == 0 {
		return st.primary.Host
	}
	healthy := make([]Target, 0, n)
	for _, t := range st.targets {
		if p.hostLive(t.Host) {
			healthy = append(healthy, t)
		}
	}
	switch len(healthy) {
	case 0:
		return st.primary.Host
	case 1:
		return healthy[0].Host
	}
	return p.pickWeighted(healthy)
}

// effectiveWeight maps an enrolled weight to the share it actually gets:
// weights below 1 mean "unweighted" and receive an equal share (weight 1),
// weights above 100 are clamped to the maximum a percentage can express.
func effectiveWeight(t Target) int {
	switch {
	case t.Weight <= 0:
		return 1
	case t.Weight > 100:
		return 100
	default:
		return t.Weight
	}
}

// pickWeighted runs one step of the smooth weighted round-robin algorithm
// (the same scheduling nginx uses) over the healthy backends. With equal
// weights it degenerates to plain round-robin, which is why rolling deploys —
// whose targets carry no weights — behave exactly as before.
func (p *Proxy) pickWeighted(healthy []Target) string {
	p.wrrMu.Lock()
	defer p.wrrMu.Unlock()

	total := 0
	for _, t := range healthy {
		total += effectiveWeight(t)
	}
	best := ""
	bestScore := int64(math.MinInt64)
	for _, t := range healthy {
		score := p.wrr[t.Host] + int64(effectiveWeight(t))
		p.wrr[t.Host] = score
		// Strictly greater: the earliest target wins ties, which keeps the
		// schedule deterministic when several backends share a weight.
		if score > bestScore {
			best, bestScore = t.Host, score
		}
	}
	p.wrr[best] -= int64(total)
	return best
}

// backendCounters is the mutable state behind one BackendStat snapshot.
type backendCounters struct {
	requests int64
	errors   int64
	// buckets is cumulative: buckets[i] counts requests within
	// LatencyBucketBounds[i]. Kept in sync under statsMu.
	buckets [len(LatencyBucketBounds)]uint64
}

// directorHostKey is the context key the Director uses to publish the backend
// chosen for this request so the counting transport can attribute statistics.
type directorHostKey struct{}

// recordBackendRequest folds one completed round trip into the backend's
// counters. status is the response's status code, or 0 when the transport
// itself failed (dial error, timeout) — those count as errors.
func (p *Proxy) recordBackendRequest(host string, status int, transportErr error, latency time.Duration) {
	if host == "" {
		return
	}
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	c := p.backendStats[host]
	if c == nil {
		c = &backendCounters{}
		if p.backendStats == nil {
			p.backendStats = make(map[string]*backendCounters)
		}
		p.backendStats[host] = c
	}
	c.requests++
	if transportErr != nil || status >= 500 {
		c.errors++
	}
	for i, bound := range LatencyBucketBounds {
		if latency <= bound {
			c.buckets[i]++
		}
	}
}

// Stats returns a snapshot of every backend's cumulative request statistics.
// Backends no longer enrolled keep their history until the daemon restarts, so
// a consumer can still diff across a membership change.
func (p *Proxy) Stats() []BackendStat {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	out := make([]BackendStat, 0, len(p.backendStats))
	labels := make(map[string]string)
	if st := p.loadState(); st != nil {
		for _, t := range st.targets {
			labels[t.Host] = t.Label
		}
		if lbl := labels[st.primary.Host]; lbl == "" && st.primary.Label != "" {
			labels[st.primary.Host] = st.primary.Label
		}
	}
	for host, c := range p.backendStats {
		out = append(out, BackendStat{
			Label:    labels[host],
			Host:     host,
			Requests: c.requests,
			Errors:   c.errors,
			Buckets:  append([]uint64(nil), c.buckets[:]...),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// hostLive consults (then refreshes) the probe cache for one backend.
func (p *Proxy) hostLive(host string) bool {
	now := time.Now()

	p.liveMu.Lock()
	if p.liveResults == nil {
		p.liveResults = make(map[string]livenessResult)
	}
	res, seen := p.liveResults[host]
	ttl := livePassTTL
	if !res.ok {
		ttl = liveFailTTL
	}
	if seen && now.Sub(res.at) < ttl {
		live := res.ok
		p.liveMu.Unlock()
		return live
	}
	p.liveMu.Unlock()

	// Probe outside the lock: dialing can block up to liveDialTO.
	conn, err := net.DialTimeout("tcp", host, liveDialTO)
	if err == nil {
		_ = conn.Close()
	}
	outcome := livenessResult{ok: err == nil, at: now}

	p.liveMu.Lock()
	p.liveResults[host] = outcome
	p.liveMu.Unlock()
	return outcome.ok
}

// New creates a Proxy bound to the given public port with an initial target.
// The public listener is NOT opened until Start is called.
func New(appName string, publicPort int, initial Target) *Proxy {
	p := &Proxy{
		PublicPort: publicPort,
		AppName:    appName,
	}
	p.state.Store(&proxyState{primary: initial, targets: []Target{initial}})

	// Build a single ReverseProxy whose Director resolves the target fresh on
	// every request. This is the crux of the zero-downtime cut-over: swapping
	// the active instance is a single atomic.Store, with no proxy rebuild, so
	// requests already copied into the ReverseProxy pipeline keep their old
	// target while the very next request picks up the new one.
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			st := p.loadState()
			// Pick the backend for this request. With a single target this is
			// exactly the previous behavior (pure primary routing, zero extra
			// work). With multiple rolling replicas it distributes new requests
			// round-robin across healthy targets; with weighted targets
			// (canary rollout) it distributes them proportionally to the
			// enrolled weights. Either way it passively skips targets that
			// recently refused connections instead of handing them a doomed
			// request.
			host := st.primary.Host
			if len(st.targets) > 1 {
				host = p.pickHealthyHost(st)
			}
			// Publish the chosen backend on the request context so the
			// counting transport can attribute per-backend statistics to it.
			*req = *req.WithContext(context.WithValue(req.Context(), directorHostKey{}, host))
			// Resolve the backend URL from the chosen target. We only rewrite
			// scheme/host/path; headers and body are preserved verbatim.
			u, err := url.Parse("http://" + host)
			if err == nil {
				req.URL.Scheme = u.Scheme
				req.URL.Host = u.Host
				// Keep the original request path (httputil sets it from req.URL).
			}
			// Preserve the inbound Host so virtual-host-aware backends work.
			req.URL.Path = singleJoiningSlash(u.Path, req.URL.Path)
			req.URL.RawPath = "" // we don't rewrite raw path; avoid mismatches
		},
		// ErrorHandler is invoked when the backend dial fails (e.g. instance
		// is mid-restart). We surface a clear 502 rather than the default text.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w,
				fmt.Sprintf("phelix proxy: upstream %q unreachable: %v",
					p.loadState().primary.Label, err),
				http.StatusBadGateway)
		},
	}
	// Wrap the default transport so we can count in-flight requests. The count
	// is best-effort and used only for observability at shutdown. The same
	// wrapper folds per-backend statistics (requests, 5xx/failed round trips,
	// latency buckets) into the proxy so a canary rollout can compare the new
	// instance against the stable baseline from real traffic.
	rp.Transport = &countingTransport{inner: http.DefaultTransport, inFlight: &p.inFlight, proxy: p}

	p.rp = rp
	return p
}

// singleJoiningSlash mirrors httputil.NewSingleHostReverseProxy's path joining.
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

func (p *Proxy) loadState() *proxyState {
	v := p.state.Load()
	if v == nil {
		// Should not happen; New always stores a value. Guard for safety.
		return &proxyState{}
	}
	return v.(*proxyState)
}

// SetTarget atomically re-points the proxy at a new primary instance. Existing
// in-flight requests continue to their already-resolved backend; the very next
// request picks up the new target. This is safe to call concurrently with
// active request serving.
//
// If addl is non-empty it replaces the full backend set (used by rolling and
// by weighted canary routing). Otherwise the backend set is rebuilt to
// {primary} for blue-green.
//
// The set is normalised: the primary is always first and duplicate hosts are
// dropped. Callers disagree on whether backends already include the primary
// (the rolling deploy path passes it in), so the daemon — the single point
// where the routing table and proxy-state.json are written — enforces
// uniqueness here; idempotent: SetTarget twice yields the same set. Weights
// travel with each target and are preserved verbatim.
func (p *Proxy) SetTarget(primary Target, addl ...Target) {
	targets := make([]Target, 0, len(addl)+1)
	targets = append(targets, primary)
	seen := map[string]bool{primary.Host: true}
	for _, t := range addl {
		if t.Host == "" || seen[t.Host] {
			continue
		}
		seen[t.Host] = true
		targets = append(targets, t)
	}
	p.state.Store(&proxyState{primary: primary, targets: targets})
	// A changed backend set invalidates the weighted round-robin schedule:
	// leftover counters from retired backends would skew the new set's shares.
	p.wrrMu.Lock()
	p.wrr = make(map[string]int64, len(targets))
	p.wrrMu.Unlock()
}

// CurrentTarget returns the primary target the proxy is routing to right now.
func (p *Proxy) CurrentTarget() Target {
	return p.loadState().primary
}

// Targets returns the full set of known backends.
func (p *Proxy) Targets() []Target {
	return p.loadState().targets
}

// InFlight returns the number of requests currently being proxied (best-effort).
func (p *Proxy) InFlight() int64 {
	return atomic.LoadInt64(&p.inFlight)
}

// Start opens the public listener and begins serving. It returns when the
// server stops (always with a non-nil error, typically http.ErrServerClosed).
// Start must be called at most once.
func (p *Proxy) Start() error {
	p.mu.Lock()
	if p.started.Swap(true) {
		p.mu.Unlock()
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "proxy: already started")
	}
	p.mu.Unlock()

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p.PublicPort))
	if err != nil {
		p.started.Store(false)
		// Preserve the underlying bind error (address in use, permission, ...)
		// so errors.Is / errors.As still reach the OS cause.
		return phelixerr.Wrapf(phelixerr.CodePortUnavailable, err, "proxy: listen on :%d", p.PublicPort)
	}
	return p.Serve(ln)
}

// Serve runs the proxy on an already-bound listener. The HTTP server is built
// synchronously so that by the time Serve is launched in the background the
// port is guaranteed to be accepting connections — callers therefore do not
// need to poll or sleep to confirm readiness.
func (p *Proxy) Serve(ln net.Listener) error {
	p.mu.Lock()
	p.server = &http.Server{
		Handler:           p.rp,
		ReadHeaderTimeout: 10 * time.Second,
	}
	p.mu.Unlock()

	return p.server.Serve(ln)
}

// Shutdown gracefully drains in-flight requests then closes the listener.
// After Shutdown the Proxy cannot be restarted.
func (p *Proxy) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	srv := p.server
	p.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// countingTransport wraps a RoundTripper to track in-flight requests and, when
// the proxy is set, per-backend request statistics.
type countingTransport struct {
	inner    http.RoundTripper
	inFlight *int64
	proxy    *Proxy
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	atomic.AddInt64(t.inFlight, 1)
	start := time.Now()
	resp, err := t.inner.RoundTrip(req)
	atomic.AddInt64(t.inFlight, -1)
	if t.proxy != nil {
		host, _ := req.Context().Value(directorHostKey{}).(string)
		status := 0
		if err == nil && resp != nil {
			status = resp.StatusCode
		}
		t.proxy.recordBackendRequest(host, status, err, time.Since(start))
	}
	return resp, err
}
