package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// DefaultSocketPath returns the path to the proxy control unix socket under
// the user's ~/.phelix directory. It matches the existing health/app layout.
func DefaultSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".phelix", "proxy.sock"), nil
}

// Op is the kind of control message exchanged over the socket.
type Op string

const (
	OpSwitch   Op = "switch"   // re-point an app's proxy at a new primary instance
	OpAdd      Op = "add"      // enrol a brand-new app (open its public port)
	OpRemove   Op = "remove"   // stop proxying for an app (close its public port)
	OpStatus   Op = "status"   // report current targets for one or all apps
	OpStats    Op = "stats"    // report per-backend request statistics for one app
	OpPing     Op = "ping"     // liveness check used by rebuild before deploys
	OpVersion  Op = "version"  // report daemon's control-protocol version
	OpShutdown Op = "shutdown" // gracefully stop the daemon (used by 'phelix proxy stop')
)

// Request is the wire format from client -> daemon.
type Request struct {
	Op         Op       `json:"op"`
	AppName    string   `json:"app_name,omitempty"`
	PublicPort int      `json:"public_port,omitempty"`
	Primary    *Target  `json:"primary,omitempty"`
	Backends   []Target `json:"backends,omitempty"`
}

// Response is the wire format from daemon -> client.
type Response struct {
	OK      bool          `json:"ok"`
	Error   string        `json:"error,omitempty"`
	Status  []AppStatus   `json:"status,omitempty"`
	Stats   []BackendStat `json:"stats,omitempty"`
	Version int           `json:"version,omitempty"`
}

// AppStatus is the per-app slice returned by OpStatus.
type AppStatus struct {
	AppName    string   `json:"app_name"`
	PublicPort int      `json:"public_port"`
	Primary    Target   `json:"primary"`
	Backends   []Target `json:"backends"`
	InFlight   int64    `json:"in_flight"`
}

// persistedApp is one entry of the proxy daemon's crash/restart state file.
type persistedApp struct {
	AppName    string   `json:"app_name"`
	PublicPort int      `json:"public_port"`
	Primary    Target   `json:"primary"`
	Backends   []Target `json:"backends,omitempty"`
}

// proxyPersistFile is the on-disk shape of ~/.phelix/proxy-state.json. The
// daemon writes it atomically after every enrollment/membership change and
// replays it on startup so a proxy restart (crash, reboot, manual restart)
// restores the public ports and active targets of every app without waiting
// for the next deploy. Without this, killing the daemon silently dropped all
// routing and left every enrolled app unreachable until redeployed.
type proxyPersistFile struct {
	Apps []persistedApp `json:"apps"`
}

// DefaultStatePath returns the path of the proxy daemon's persistence file.
func DefaultStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".phelix", "proxy-state.json"), nil
}

// Daemon hosts one Proxy per enrolled app plus the control socket.
type Daemon struct {
	socketPath string

	mu      sync.Mutex
	proxies map[string]*Proxy // keyed by AppName

	listener net.Listener
	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewDaemon creates a Daemon bound to the given unix socket path.
func NewDaemon(socketPath string) *Daemon {
	return &Daemon{
		socketPath: socketPath,
		proxies:    make(map[string]*Proxy),
		stopCh:     make(chan struct{}),
	}
}

// Run opens the control socket and serves control requests until Shutdown is
// called or the socket is closed. Each enrolled app's Proxy runs in its own
// goroutine; a failed Serve (e.g. listener closed) logs and the app proxy is
// left for Shutdown to clean up.
//
// Before accepting control traffic the daemon restores any previously enrolled
// apps from ~/.phelix/proxy-state.json: their public ports are rebound and
// their last-known targets reinstated, so backend instances (which kept
// running while the daemon was down) are reachable again immediately.
func (d *Daemon) Run() error {
	d.restoreState()

	_ = os.Remove(d.socketPath) // best-effort: clear any stale socket
	if err := os.MkdirAll(filepath.Dir(d.socketPath), 0o755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "proxy: create socket dir")
	}
	ln, err := net.Listen("unix", d.socketPath)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodePortUnavailable, err, "proxy: listen %s", d.socketPath)
	}
	d.listener = ln

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-d.stopCh:
					return // Shutdown in progress
				default:
				}
				continue
			}
			d.wg.Add(1)
			go func(c net.Conn) {
				defer d.wg.Done()
				defer c.Close()
				d.handleConn(c)
			}(conn)
		}
	}()
	return nil
}

// Shutdown stops the control socket and every app proxy, draining in-flight
// requests up to the given grace period.
func (d *Daemon) Shutdown(ctx context.Context) error {
	d.stopOnce.Do(func() { close(d.stopCh) })
	if d.listener != nil {
		_ = d.listener.Close()
	}

	d.mu.Lock()
	proxies := make([]*Proxy, 0, len(d.proxies))
	for _, p := range d.proxies {
		proxies = append(proxies, p)
	}
	d.mu.Unlock()

	var firstErr error
	for _, p := range proxies {
		if err := p.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	d.wg.Wait()
	_ = os.Remove(d.socketPath)
	return firstErr
}

// startAppProxy binds the app's public port SYNCHRONOUSLY and starts serving
// in the background. Callers can rely on the port accepting connections as
// soon as this returns — no readiness polling anywhere.
func (d *Daemon) startAppProxy(p *Proxy) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p.PublicPort))
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodePortUnavailable, err,
			"proxy: bind public port %d", p.PublicPort)
	}
	go func() {
		// The Director resolves targets per request; serve errors here are
		// only listener teardown, logged for observability.
		if serr := p.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "[proxy] app %s stopped serving: %v\n", p.AppName, serr)
			d.mu.Lock()
			if cur, ok := d.proxies[p.AppName]; ok && cur == p {
				delete(d.proxies, p.AppName)
			}
			d.mu.Unlock()
		}
	}()
	return nil
}

// EnrollApp adds an app to the daemon and starts proxying on its public port.
//
// Re-enrolling an app that already exists on the SAME public port atomically
// updates its target set and keeps the bound listener untouched — there is no
// close/reopen gap during which clients could get connection refused. Only a
// genuine public-port change takes down and rebinds the old listener.
func (d *Daemon) EnrollApp(appName string, publicPort int, primary Target, backends []Target) error {
	if appName == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "proxy: appName is required")
	}
	if publicPort <= 0 {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "proxy: valid public port required")
	}
	if primary.Host == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "proxy: primary target host is required")
	}

	d.mu.Lock()
	existing := d.proxies[appName]
	d.mu.Unlock()

	if existing != nil && existing.PublicPort == publicPort {
		// Same public port: swap membership under the live listener.
		existing.SetTarget(primary, backends...)
		d.persistState()
		return nil
	}

	p := New(appName, publicPort, primary)
	if len(backends) > 0 {
		p.SetTarget(primary, backends...)
	}

	// Bind BEFORE publishing so a failed bind leaves the daemon's map (and
	// persisted state) untouched.
	if err := d.startAppProxy(p); err != nil {
		return err
	}

	d.mu.Lock()
	old := d.proxies[appName]
	d.proxies[appName] = p
	d.mu.Unlock()

	if old != nil {
		// Public port changed: retire the previous listener.
		_ = old.Shutdown(context.Background())
	}
	d.persistState()
	return nil
}

// SwitchApp atomically re-points an enrolled app's proxy at a new primary.
func (d *Daemon) SwitchApp(appName string, primary Target, backends []Target) error {
	d.mu.Lock()
	p, ok := d.proxies[appName]
	d.mu.Unlock()
	if !ok {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "proxy: app %q is not enrolled with the proxy daemon", appName)
	}
	if len(backends) > 0 {
		p.SetTarget(primary, backends...)
	} else {
		p.SetTarget(primary)
	}
	d.persistState()
	return nil
}

// RemoveApp stops proxying for an app and closes its public listener.
func (d *Daemon) RemoveApp(appName string) error {
	d.mu.Lock()
	p, ok := d.proxies[appName]
	if ok {
		delete(d.proxies, appName)
	}
	d.mu.Unlock()
	if !ok {
		return nil
	}
	err := p.Shutdown(context.Background())
	// Only forget the enrollment from disk once the listener actually closed;
	// on shutdown failure the app keeps serving through the old proxy, so the
	// persisted state must keep describing it.
	if err == nil {
		d.persistState()
	}
	return err
}

// snapshotPersist builds the current routing state under the daemon lock.
func (d *Daemon) snapshotPersist() proxyPersistFile {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := proxyPersistFile{Apps: make([]persistedApp, 0, len(d.proxies))}
	for name, p := range d.proxies {
		st := p.loadState()
		out.Apps = append(out.Apps, persistedApp{
			AppName:    name,
			PublicPort: p.PublicPort,
			Primary:    st.primary,
			Backends:   append([]Target(nil), st.targets...),
		})
	}
	sort.Slice(out.Apps, func(i, j int) bool { return out.Apps[i].AppName < out.Apps[j].AppName })
	return out
}

// persistState atomically writes the current routing state so a daemon
// restart can restore every enrollment. Failures are logged, never fatal:
// losing persistence degrades to the old "state lost after restart" behavior
// instead of breaking live traffic handling.
func (d *Daemon) persistState() {
	path, err := DefaultStatePath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[proxy] state path unavailable: %v\n", err)
		return
	}
	data, err := json.MarshalIndent(d.snapshotPersist(), "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[proxy] encode state: %v\n", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "[proxy] state dir: %v\n", err)
		return
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "[proxy] write state: %v\n", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "[proxy] commit state: %v\n", err)
	}
}

// reconcileBackends reconciles persisted routing state with runtime reality
// before a daemon restart restores it: backends that no longer accept
// connections are dropped so the restored proxy never routes to a dead port,
// and an unreachable primary falls back to the first live backend. When no
// backend is reachable the persisted set is kept untouched — every instance
// may simply still be starting (e.g. after a reboot, instances come back via
// 'phelix start' after the daemon), and wiping the route would make the
// daemon forget the app entirely.
func reconcileBackends(primary Target, backends []Target) (Target, []Target) {
	if len(backends) == 0 {
		return primary, backends
	}
	dialable := func(host string) bool {
		if host == "" {
			return false
		}
		conn, err := net.DialTimeout("tcp", host, liveDialTO)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	live := make([]Target, 0, len(backends))
	for _, b := range backends {
		if dialable(b.Host) {
			live = append(live, b)
		}
	}
	if len(live) == 0 || len(live) == len(backends) {
		return primary, backends
	}
	if !dialable(primary.Host) {
		primary = live[0]
	}
	return primary, live
}

// restoreState replays ~/.phelix/proxy-state.json: each recorded app's public
// port is rebound and its last-known targets reinstated. Apps whose ports no
// longer bind are skipped (logged) — one conflicting port must never take down
// recovery of the others — and the file is rewritten to describe reality.
func (d *Daemon) restoreState() {
	path, err := DefaultStatePath()
	if err != nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return // first run or unreadable file: start empty
	}
	var pf proxyPersistFile
	if err := json.Unmarshal(data, &pf); err != nil {
		fmt.Fprintf(os.Stderr, "[proxy] ignoring corrupt %s: %v\n", path, err)
		return
	}

	recovered := 0
	var lost []string
	for _, app := range pf.Apps {
		if app.AppName == "" || app.PublicPort <= 0 || app.Primary.Host == "" {
			continue
		}
		primary, backends := reconcileBackends(app.Primary, app.Backends)
		p := New(app.AppName, app.PublicPort, primary)
		if len(backends) > 0 {
			p.SetTarget(primary, backends...)
		}
		if err := d.startAppProxy(p); err != nil {
			fmt.Fprintf(os.Stderr, "[proxy] restore %s: %v (skipped)\n", app.AppName, err)
			lost = append(lost, app.AppName)
			continue
		}
		d.mu.Lock()
		d.proxies[app.AppName] = p
		d.mu.Unlock()
		recovered++
	}
	if recovered > 0 {
		fmt.Fprintf(os.Stderr, "[proxy] restored %d enrolled app(s) from previous run\n", recovered)
	}
	if len(lost) > 0 {
		// Rewrite the state without the apps we could not rebind so the file
		// matches what is actually serving.
		d.persistState()
	}
}

// Status reports the current routing state for one app (or all if name is "").
func (d *Daemon) Status(appName string) []AppStatus {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]AppStatus, 0, len(d.proxies))
	for name, p := range d.proxies {
		if appName != "" && name != appName {
			continue
		}
		out = append(out, AppStatus{
			AppName:    name,
			PublicPort: p.PublicPort,
			Primary:    p.CurrentTarget(),
			Backends:   p.Targets(),
			InFlight:   p.InFlight(),
		})
	}
	return out
}

// Stats reports per-backend request statistics for one app (or all apps if
// name is ""). Statistics are cumulative since the daemon started.
func (d *Daemon) Stats(appName string) []BackendStat {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]BackendStat, 0, len(d.proxies))
	for name, p := range d.proxies {
		if appName != "" && name != appName {
			continue
		}
		out = append(out, p.Stats()...)
	}
	return out
}

// handleConn processes one control connection: one JSON Request -> one Response.
func (d *Daemon) handleConn(conn net.Conn) {
	reader := bufio.NewReader(conn)
	data, err := reader.ReadBytes('\n')
	if err != nil {
		if !errors.Is(err, io.EOF) {
			fmt.Fprintf(os.Stderr, "[proxy] control read: %v\n", err)
		}
		return
	}

	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		writeResponse(conn, Response{OK: false, Error: "invalid request: " + err.Error()})
		return
	}

	switch req.Op {
	case OpPing:
		writeResponse(conn, Response{OK: true})

	case OpVersion:
		writeResponse(conn, Response{OK: true, Version: proxyProtoVersion})

	case OpAdd:
		if req.Primary == nil {
			writeResponse(conn, Response{OK: false, Error: "primary is required for add"})
			return
		}
		if err := d.EnrollApp(req.AppName, req.PublicPort, *req.Primary, req.Backends); err != nil {
			writeResponse(conn, Response{OK: false, Error: err.Error()})
			return
		}
		writeResponse(conn, Response{OK: true})

	case OpSwitch:
		if req.Primary == nil {
			writeResponse(conn, Response{OK: false, Error: "primary is required for switch"})
			return
		}
		if err := d.SwitchApp(req.AppName, *req.Primary, req.Backends); err != nil {
			writeResponse(conn, Response{OK: false, Error: err.Error()})
			return
		}
		writeResponse(conn, Response{OK: true})

	case OpRemove:
		if err := d.RemoveApp(req.AppName); err != nil {
			writeResponse(conn, Response{OK: false, Error: err.Error()})
			return
		}
		writeResponse(conn, Response{OK: true})

	case OpStatus:
		writeResponse(conn, Response{OK: true, Status: d.Status(req.AppName)})

	case OpStats:
		writeResponse(conn, Response{OK: true, Stats: d.Stats(req.AppName)})

	case OpShutdown:
		// Acknowledge first so the client gets a clean reply before we tear
		// the socket down. Shutdown runs asynchronously with a short grace.
		writeResponse(conn, Response{OK: true})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = d.Shutdown(ctx)
			os.Exit(0)
		}()

	default:
		writeResponse(conn, Response{OK: false, Error: "unknown op: " + string(req.Op)})
	}
}

func writeResponse(w io.Writer, resp Response) {
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	b = append(b, '\n')
	_, _ = w.Write(b)
}

// Client talks to a running Daemon over the control socket.
type Client struct {
	socketPath string
	timeout    time.Duration
}

// NewClient returns a Client for the socket at socketPath.
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath, timeout: 5 * time.Second}
}

// IsRunning reports whether a daemon is listening on the socket.
func (c *Client) IsRunning() bool {
	conn, err := net.DialTimeout("unix", c.socketPath, c.timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Do sends a single Request and returns the Response.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	dialer := net.Dialer{Timeout: c.timeout}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConnection, err, "proxy: dial control socket (is 'phelix proxy' running?)")
	}
	defer conn.Close()

	// Honor ctx deadlines for the full round-trip if present.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(c.timeout))
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConnection, err, "proxy: write control socket")
	}

	reader := bufio.NewReader(conn)
	data, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConnection, err, "proxy: read control socket")
	}
	var resp Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "proxy: decode control response")
	}
	return &resp, nil
}

// Convenience wrappers for the common ops.

// Ping liveness-checks the daemon.
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.Do(ctx, Request{Op: OpPing})
	if err != nil {
		return err
	}
	if !resp.OK {
		return phelixerr.Newf(phelixerr.CodeProxy, "proxy: ping failed: %s", resp.Error)
	}
	return nil
}

// Add enrols a new app.
func (c *Client) Add(ctx context.Context, appName string, publicPort int, primary Target, backends ...Target) error {
	resp, err := c.Do(ctx, Request{Op: OpAdd, AppName: appName, PublicPort: publicPort, Primary: &primary, Backends: backends})
	if err != nil {
		return err
	}
	if !resp.OK {
		return phelixerr.Newf(phelixerr.CodeProxy, "proxy: add failed: %s", resp.Error)
	}
	return nil
}

// Switch atomically re-points an app's proxy at a new primary.
func (c *Client) Switch(ctx context.Context, appName string, primary Target, backends ...Target) error {
	resp, err := c.Do(ctx, Request{Op: OpSwitch, AppName: appName, Primary: &primary, Backends: backends})
	if err != nil {
		return err
	}
	if !resp.OK {
		return phelixerr.Newf(phelixerr.CodeProxy, "proxy: switch failed: %s", resp.Error)
	}
	return nil
}

// Remove stops proxying for an app.
func (c *Client) Remove(ctx context.Context, appName string) error {
	resp, err := c.Do(ctx, Request{Op: OpRemove, AppName: appName})
	if err != nil {
		return err
	}
	if !resp.OK {
		return phelixerr.Newf(phelixerr.CodeProxy, "proxy: remove failed: %s", resp.Error)
	}
	return nil
}

// Status returns routing state. Pass "" for all apps.
func (c *Client) Status(ctx context.Context, appName string) ([]AppStatus, error) {
	resp, err := c.Do(ctx, Request{Op: OpStatus, AppName: appName})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, phelixerr.Newf(phelixerr.CodeProxy, "proxy: status failed: %s", resp.Error)
	}
	return resp.Status, nil
}

// Stats returns per-backend request statistics for one app ("" for all apps).
// The counters are cumulative since the daemon started; diff two snapshots to
// get windowed rates (the canary rollout verifier does exactly that).
func (c *Client) Stats(ctx context.Context, appName string) ([]BackendStat, error) {
	resp, err := c.Do(ctx, Request{Op: OpStats, AppName: appName})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, phelixerr.Newf(phelixerr.CodeProxy, "proxy: stats failed: %s", resp.Error)
	}
	return resp.Stats, nil
}

// Shutdown asks a running daemon to drain and exit. Used by 'phelix proxy stop'.
func (c *Client) Shutdown(ctx context.Context) error {
	resp, err := c.Do(ctx, Request{Op: OpShutdown})
	if err != nil {
		return err
	}
	if !resp.OK {
		return phelixerr.Newf(phelixerr.CodeProxy, "proxy: shutdown failed: %s", resp.Error)
	}
	// Wait briefly for the socket to disappear so callers can report success.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !c.IsRunning() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// isPortOpen is a tiny TCP probe used to confirm the public listener bound.
func isPortOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// proxyProtoVersion is the daemon control-protocol version. It is bumped when
// daemon behavior changes in a way an old, still-running daemon cannot honor
// (e.g. backend-set dedupe, weighted routing, per-backend stats).
// EnsureDaemon compares it against the running daemon's version and restarts
// stale daemons so a rebuilt CLI never keeps talking to a pre-upgrade proxy
// that would persist outdated state.
//
// v3 added: Target.Weight on Add/Switch (an older daemon silently drops the
// field and would split canary traffic 50/50) and the OpStats per-backend
// metrics op (an older daemon answers "unknown op").
const proxyProtoVersion = 3

// daemonVersionOf returns the running daemon's protocol version. A daemon
// older than the version handshake answers OpVersion with an unknown-op
// error; treat that as version 1.
func daemonVersionOf(ctx context.Context, c *Client) int {
	resp, err := c.Do(ctx, Request{Op: OpVersion})
	if err != nil || !resp.OK {
		return 1
	}
	return resp.Version
}

// EnsureDaemon checks whether a proxy daemon is reachable on the control
// socket; if not, it launches `phelix proxy` detached in the background and
// waits (up to wait) for the socket to start answering. It is what makes
// `phelix rebuild --blue-green` "just work" without the user having to start
// the daemon by hand.
//
//   - phelixBin is the absolute path to the phelix executable (usually
//     os.Executable()); pass "" to look it up via os.Args[0].
//   - wait is how long to poll the socket before giving up; <=0 uses 5s.
//
// It returns nil if, on return, the daemon answers a Ping.
func EnsureDaemon(ctx context.Context, phelixBin string, wait time.Duration) error {
	socket, err := DefaultSocketPath()
	if err != nil {
		return err
	}
	client := NewClient(socket)

	// Fast path: already running. An old daemon left over from a previous
	// phelix build must not keep serving stale proxy logic (e.g. persisting
	// duplicate backends): if its protocol version predates the current one,
	// replace it before the caller enrolls new state into it.
	if client.IsRunning() {
		if daemonVersionOf(ctx, client) >= proxyProtoVersion {
			return nil
		}
		// Stale daemon: drain (shutdown drains up to 30s) and replace it with
		// a fresh one from this binary.
		_, _ = client.Do(ctx, Request{Op: OpShutdown})
		staleDeadline := time.Now().Add(35 * time.Second)
		for client.IsRunning() && time.Now().Before(staleDeadline) {
			time.Sleep(100 * time.Millisecond)
		}
		// Give the kernel a beat to release the control socket.
		time.Sleep(200 * time.Millisecond)
	}

	// Resolve the binary to launch.
	if phelixBin == "" {
		if exe, err := os.Executable(); err == nil {
			phelixBin = exe
		} else {
			phelixBin = os.Args[0]
		}
	}
	if _, err := os.Stat(phelixBin); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeProxy, err, "proxy: cannot locate phelix executable to start daemon")
	}

	// Spawn detached: own session, stdio to /dev/null, survive this CLI exit.
	// --foreground is required so the child actually runs the daemon loop
	// instead of trying to background itself again.
	cmd := exec.Command(phelixBin, "proxy", "--foreground")
	cmd.SysProcAttr = detachedSysProcAttr()
	// Redirect stdio so the background process never touches our terminal.
	if devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		cmd.Stdin = devnull
		cmd.Stdout = devnull
		cmd.Stderr = devnull
	}
	if err := cmd.Start(); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeProxy, err, "proxy: failed to start daemon")
	}
	// Release the child so it isn't reaped/killed when this process exits.
	_ = cmd.Process.Release()

	if wait <= 0 {
		wait = 5 * time.Second
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if client.IsRunning() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return phelixerr.Newf(phelixerr.CodeTimeout, "proxy: daemon did not become reachable on %s within %s", socket, wait)
}
