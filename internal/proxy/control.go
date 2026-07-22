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
	"sync"
	"time"
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
	OpPing     Op = "ping"     // liveness check used by rebuild before deploys
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
	OK     bool        `json:"ok"`
	Error  string      `json:"error,omitempty"`
	Status []AppStatus `json:"status,omitempty"`
}

// AppStatus is the per-app slice returned by OpStatus.
type AppStatus struct {
	AppName    string   `json:"app_name"`
	PublicPort int      `json:"public_port"`
	Primary    Target   `json:"primary"`
	Backends   []Target `json:"backends"`
	InFlight   int64    `json:"in_flight"`
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
func (d *Daemon) Run() error {
	_ = os.Remove(d.socketPath) // best-effort: clear any stale socket
	if err := os.MkdirAll(filepath.Dir(d.socketPath), 0o755); err != nil {
		return fmt.Errorf("proxy: create socket dir: %w", err)
	}
	ln, err := net.Listen("unix", d.socketPath)
	if err != nil {
		return fmt.Errorf("proxy: listen %s: %w", d.socketPath, err)
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

// EnrollApp adds an app to the daemon and starts proxying on its public port.
// If an app with the same name exists it is replaced (the old listener closed).
func (d *Daemon) EnrollApp(appName string, publicPort int, primary Target, backends []Target) error {
	if appName == "" {
		return errors.New("appName is required")
	}
	if primary.Host == "" {
		return errors.New("primary target host is required")
	}

	p := New(appName, publicPort, primary)
	if len(backends) > 0 {
		p.SetTarget(primary, backends...)
	}

	d.mu.Lock()
	if old, ok := d.proxies[appName]; ok {
		d.mu.Unlock()
		_ = old.Shutdown(context.Background())
		d.mu.Lock()
	}
	d.proxies[appName] = p
	d.mu.Unlock()

	// Serve in the background; the request Director resolves the target per
	// request, so it keeps routing correctly even as SetTarget is called.
	go func() {
		if err := p.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// The public listener failed. Remove the app so a later EnrollApp
			// can retry; surface via stderr since the daemon is long-running.
			fmt.Fprintf(os.Stderr, "[proxy] app %s stopped serving: %v\n", appName, err)
			d.mu.Lock()
			if cur, ok := d.proxies[appName]; ok && cur == p {
				delete(d.proxies, appName)
			}
			d.mu.Unlock()
		}
	}()

	// Give the listener a beat to actually bind so a client that immediately
	// sends traffic right after enrol works in practice.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if isPortOpen(publicPort) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// SwitchApp atomically re-points an enrolled app's proxy at a new primary.
func (d *Daemon) SwitchApp(appName string, primary Target, backends []Target) error {
	d.mu.Lock()
	p, ok := d.proxies[appName]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("app %q is not enrolled with the proxy daemon", appName)
	}
	if len(backends) > 0 {
		p.SetTarget(primary, backends...)
	} else {
		p.SetTarget(primary)
	}
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
	return p.Shutdown(context.Background())
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
		return nil, fmt.Errorf("proxy: dial control socket: %w (is 'phelix proxy' running?)", err)
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
		return nil, fmt.Errorf("proxy: write control socket: %w", err)
	}

	reader := bufio.NewReader(conn)
	data, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("proxy: read control socket: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("proxy: decode control response: %w", err)
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
		return errors.New("proxy: ping failed: " + resp.Error)
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
		return errors.New("proxy: add failed: " + resp.Error)
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
		return errors.New("proxy: switch failed: " + resp.Error)
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
		return errors.New("proxy: remove failed: " + resp.Error)
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
		return nil, errors.New("proxy: status failed: " + resp.Error)
	}
	return resp.Status, nil
}

// Shutdown asks a running daemon to drain and exit. Used by 'phelix proxy stop'.
func (c *Client) Shutdown(ctx context.Context) error {
	resp, err := c.Do(ctx, Request{Op: OpShutdown})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New("proxy: shutdown failed: " + resp.Error)
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

	// Fast path: already running.
	if client.IsRunning() {
		return nil
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
		return fmt.Errorf("proxy: cannot locate phelix executable to start daemon: %w", err)
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
		return fmt.Errorf("proxy: failed to start daemon: %w", err)
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
	return fmt.Errorf("proxy: daemon did not become reachable on %s within %s", socket, wait)
}
