package grpc

import (
	"context"
	"crypto/tls"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/config"
	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/connstate"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

const (
	keepaliveInterval    = 10 * time.Second
	keepaliveTimeout     = 3 * time.Second
	connectionTimeout    = 10 * time.Second
	metadataSyncInterval = 30 * time.Second
)

// Client manages the gRPC connection to the backend.
type Client struct {
	conn          *grpc.ClientConn
	serviceClient pb.PhelixServiceClient
	mu            sync.RWMutex
	done          chan struct{}
	reconnectCh   chan struct{}
	reconnect     *reconnectState
	connected     bool
	cancel        context.CancelFunc
	unreadySince  time.Time
}

// NewClient creates a new gRPC client instance.
func NewClient() *Client {
	return &Client{
		done:        make(chan struct{}),
		reconnectCh: make(chan struct{}, 1),
		reconnect:   newReconnectState(),
	}
}

// transportCredentials selects the gRPC transport security for the
// configured backend. Production traffic always uses TLS; only explicit
// "dev" mode (see config.yml) is allowed to fall back to plaintext, and only
// for local development against a backend that doesn't terminate TLS.
//
// This is intentionally NOT configurable from the production config file —
// there is no insecure fallback for the production backend.
func transportCredentials() credentials.TransportCredentials {
	cfg := config.Get()
	if cfg != nil && cfg.App.Mode == "dev" {
		logs.InfoFile("grpc", "[gRPC] dev mode: using insecure (plaintext) transport credentials")
		return insecure.NewCredentials()
	}
	// #nosec G402 -- default TLS config: verifies the server certificate
	// against the system trust store, which is what we want for the
	// production backend (no InsecureSkipVerify, no custom CA override).
	return credentials.NewTLS(&tls.Config{})
}

// Connect establishes the gRPC connection to the backend.
func (c *Client) Connect() error {
	cfg := config.Get()
	if cfg == nil {
		// No config loaded yet — this client is not configured to talk to
		// any backend. Return a proper error instead of dereferencing a nil
		// pointer; callers that run in the background (e.g. the rollback
		// event sender) must be able to handle the failure.
		return phelixerr.New(phelixerr.CodeConfiguration, "gRPC client has no configuration loaded")
	}
	if cfg.App.GRPCUrl == "" {
		logs.WarningFile("grpc", "[gRPC] No gRPC URL configured, skipping connection")
		return nil
	}

	// No session → the backend would reject every RPC for lack of auth. Skip
	// connecting entirely so short-lived CLI commands (build/rebuild/start)
	// don't block on a doomed dial. Callers that keep retrying (the monitor
	// daemon, rollback sender) will pick up the session automatically once the
	// user runs 'phelix auth login'.
	if !sessionAvailable() {
		// Logged out (or never logged in) while this runtime is up: record why
		// dashboard sync is off, separately from the local runtime status.
		connstate.MarkDisconnected()
		return phelixerr.New(
			phelixerr.CodeUnauthenticated,
			"not logged in; skipping gRPC connection to dashboard",
		)
	}

	// Initialize server if not already done (needed for CLI commands)
	if err := server.Initialize(); err != nil {
		logs.ErrorFile("grpc", "[gRPC] Failed to initialize server: %v", err)
		return nil
	}

	serverID := server.GetServerID()
	if serverID == "" {
		logs.WarningFile("grpc", "[gRPC] No server ID available after initialization")
		return phelixerr.New(phelixerr.CodeServer, "no server ID available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectionTimeout)
	defer cancel()

	// Create auth context
	authCtx, err := attachAuthMetadata(ctx, serverID)
	if err != nil {
		return err
	}

	conn, err := grpc.DialContext(
		authCtx,
		cfg.App.GRPCUrl,
		grpc.WithTransportCredentials(transportCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                keepaliveInterval,
			Timeout:             keepaliveTimeout,
			PermitWithoutStream: true,
		}),
		grpc.WithBlock(),
	)
	if err != nil {
		return phelixerr.Wrapf(
			phelixerr.CodeConnection,
			err,
			"failed to connect to %s",
			cfg.App.GRPCUrl,
		)
	}

	c.mu.Lock()
	oldConn := c.conn
	c.conn = conn
	c.serviceClient = pb.NewPhelixServiceClient(conn)
	c.connected = true
	c.unreadySince = time.Time{}
	c.mu.Unlock()

	// A full redial replaces the ClientConn outright. Close the previous one
	// (if any) so its background transport/backoff goroutines don't leak.
	if oldConn != nil && oldConn != conn {
		_ = oldConn.Close()
	}

	logs.InfoFile("grpc", "[gRPC] Connected to %s", cfg.App.GRPCUrl)
	connstate.MarkConnected()
	return nil
}

// Start initializes the gRPC client and begins background operations.
func (c *Client) Start() {
	if err := c.Connect(); err != nil {
		logs.ErrorFile("grpc", "[gRPC] Initial connection failed: %v, will retry", err)
		c.scheduleReconnect()
	}

	// Start reconnect loop
	go c.startReconnectLoop()

	// Start metadata sync loop
	go c.startMetadataSync()

	// Start version list sync loop
	go c.startVersionSync()

	// Start connection health monitor
	go c.monitorConnection()

	// Start agent stream for backend-to-CLI commands
	go c.startAgentStream()

	// Start the persistent monitoring stream (replaces the legacy
	// WebSocket monitor). Runs for the lifetime of the client.
	c.StartMonitorStream()
}

// Close gracefully shuts down the gRPC client.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case <-c.done:
		// Already closed
		return
	default:
		close(c.done)
	}

	// Stop agent stream
	c.stopAgentStream()

	// Stop the monitoring stream
	stopMonitorStream()

	if c.conn != nil {
		if err := c.conn.Close(); err != nil {
			logs.ErrorFile("grpc", "[gRPC] Error closing connection: %v", err)
		}
	}

	c.connected = false
	logs.InfoFile("grpc", "[gRPC] Client closed")
}

// IsConnected returns whether the gRPC client has an active connection.
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected && c.conn != nil && c.conn.GetState() == connectivity.Ready
}

// GetServiceClient returns the underlying PhelixServiceClient.
func (c *Client) GetServiceClient() pb.PhelixServiceClient {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serviceClient
}

// staleConnectionThreshold is how long the connection can sit in a
// non-Ready state (despite being nudged) before we give up waiting on
// gRPC's own transport-level recovery and force a brand-new dial. It is a
// var (not a const) so tests can shorten it instead of waiting on the real
// threshold.
var staleConnectionThreshold = 45 * time.Second

// reconnectIfNeeded checks connection state and triggers reconnect if
// needed. It handles two distinct failure modes:
//
//  1. The ClientConn is gone/shut down entirely -> full redial.
//  2. The ClientConn exists but isn't Ready (TransientFailure, or Idle —
//     which gRPC only exits via an explicit Connect() call or a new RPC).
//     We nudge it via conn.Connect() first, which is cheap and reuses all
//     existing backoff state. If that doesn't bring it back to Ready
//     within staleConnectionThreshold, we escalate to a full redial.
func (c *Client) reconnectIfNeeded() {
	c.mu.Lock()
	conn := c.conn

	if conn == nil {
		c.mu.Unlock()
		c.scheduleReconnect()
		return
	}

	state := conn.GetState()
	if state == connectivity.Ready {
		c.unreadySince = time.Time{}
		c.mu.Unlock()
		return
	}

	if c.unreadySince.IsZero() {
		c.unreadySince = time.Now()
	}
	stuckFor := time.Since(c.unreadySince)
	c.mu.Unlock()

	if state == connectivity.Shutdown {
		logs.DebugFile("grpc", "[gRPC] Connection state: %v, scheduling reconnect", state)
		c.scheduleReconnect()
		return
	}

	// TransientFailure and Idle both mean "not currently connected", but
	// gRPC's own background reconnection may be paused (e.g. an idle
	// ClientConn only resumes connecting when Connect() or an RPC is
	// invoked). Nudge it directly rather than waiting indefinitely.
	logs.WarningFile("grpc", "[gRPC] Connection state: %v (unready for %s), nudging", state, stuckFor.Round(time.Second))
	conn.Connect()

	// If nudging repeatedly hasn't helped within the threshold, the
	// ClientConn itself may be wedged. Escalate to a full redial.
	if stuckFor >= staleConnectionThreshold {
		logs.WarningFile("grpc", "[gRPC] Connection stuck in %v for %s, forcing full reconnect", state, stuckFor.Round(time.Second))
		c.mu.Lock()
		c.unreadySince = time.Time{}
		c.mu.Unlock()
		c.scheduleReconnect()
	}
}

// monitorConnection periodically checks the connection health. The interval
// is short so a stuck/idle connection is detected and nudged quickly rather
// than silently waiting on gRPC's own transport-level recovery.
func (c *Client) monitorConnection() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.reconnectIfNeeded()
		}
	}
}

// startMetadataSync periodically sends metadata to the backend.
func (c *Client) startMetadataSync() {
	ticker := time.NewTicker(metadataSyncInterval)
	defer ticker.Stop()

	// Send initial metadata immediately
	c.sendMetadataOnce()

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.sendMetadataOnce()
		}
	}
}

func (c *Client) sendMetadataOnce() {
	if !c.IsConnected() {
		return
	}

	md := collectMetadata()
	if md == nil {
		logs.WarningFile("grpc", "[gRPC] collectMetadata returned nil (server_id empty?)")
		return
	}

	logs.InfoFile("grpc", "[gRPC] Syncing metadata: server_id=%s apps=%d running=%d", md.GetServerId(), md.GetTotalManagedApps(), md.GetRunningApps())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverID := server.GetServerID()
	authCtx, err := attachAuthMetadata(ctx, serverID)
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC] Failed to attach auth metadata: %v", err)
		return
	}

	resp, err := c.serviceClient.SyncMetadata(authCtx, md)
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC] Metadata sync failed: %v", err)
		// A credentials rejection is not a network failure: retrying forever
		// with the same revoked key would hammer the backend. Stop
		// authenticated sync (streams keep failing fast) and record why, but
		// leave the local runtime and managed apps untouched. 'phelix auth
		// login' restores normal operation with the same agent_id.
		code := phelixerr.CodeOf(phelixerr.FromGRPC(err))
		if code == phelixerr.CodeUnauthenticated || code == phelixerr.CodeSessionExpired {
			c.handleAuthRejected()
			return
		}
		c.reconnectIfNeeded()
		return
	}

	if !resp.Accepted {
		logs.ErrorFile("grpc", "[gRPC] Metadata rejected: %s", resp.Message)
	}
}

// startVersionSync periodically sends version lists for all managed apps.
func (c *Client) startVersionSync() {
	// Use a longer interval than metadata sync — version lists change less
	// frequently and carry more data.
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	// Send initial sync after a short delay to let the connection stabilize.
	select {
	case <-c.done:
		return
	case <-time.After(5 * time.Second):
		c.syncAllVersions()
	}

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.syncAllVersions()
		}
	}
}

func (c *Client) syncAllVersions() {
	if !c.IsConnected() {
		return
	}

	apps := app.Manager.ListApplications()
	for _, a := range apps {
		if a.Directory == "" {
			continue
		}
		SendVersionListForApp(a.ID, a.Name, a.Directory)
	}
}
