package grpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/config"
	"github.com/abdorrahmani/phelix/internal/app"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
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
		grpcLog("[gRPC] dev mode: using insecure (plaintext) transport credentials")
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
	if cfg.App.GRPCUrl == "" {
		grpcLog("[gRPC] No gRPC URL configured, skipping connection")
		return nil
	}

	// Initialize server if not already done (needed for CLI commands)
	if err := server.Initialize(); err != nil {
		grpcLog("[gRPC] Failed to initialize server: %v", err)
		return nil
	}

	serverID := server.GetServerID()
	if serverID == "" {
		grpcLog("[gRPC] No server ID available after initialization")
		return fmt.Errorf("no server ID available")
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
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.serviceClient = pb.NewPhelixServiceClient(conn)
	c.connected = true
	c.mu.Unlock()

	grpcLog("[gRPC] Connected to %s", cfg.App.GRPCUrl)
	return nil
}

// Start initializes the gRPC client and begins background operations.
func (c *Client) Start() {
	if err := c.Connect(); err != nil {
		grpcLog("[gRPC] Initial connection failed: %v, will retry", err)
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
			grpcLog("[gRPC] Error closing connection: %v", err)
		}
	}

	c.connected = false
	grpcLog("[gRPC] Client closed")
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

// reconnectIfNeeded checks connection state and triggers reconnect if needed.
func (c *Client) reconnectIfNeeded() {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		c.scheduleReconnect()
		return
	}

	state := conn.GetState()
	if state == connectivity.Shutdown || state == connectivity.TransientFailure {
		grpcLog("[gRPC] Connection state: %v, scheduling reconnect", state)
		c.scheduleReconnect()
	}
}

// monitorConnection periodically checks the connection health.
func (c *Client) monitorConnection() {
	ticker := time.NewTicker(15 * time.Second)
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
		grpcLog("[gRPC] collectMetadata returned nil (server_id empty?)")
		return
	}

	grpcLog("[gRPC] Syncing metadata: server_id=%s apps=%d running=%d", md.GetServerId(), md.GetTotalManagedApps(), md.GetRunningApps())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverID := server.GetServerID()
	authCtx, err := attachAuthMetadata(ctx, serverID)
	if err != nil {
		grpcLog("[gRPC] Failed to attach auth metadata: %v", err)
		return
	}

	resp, err := c.serviceClient.SyncMetadata(authCtx, md)
	if err != nil {
		grpcLog("[gRPC] Metadata sync failed: %v", err)
		c.reconnectIfNeeded()
		return
	}

	if !resp.Accepted {
		grpcLog("[gRPC] Metadata rejected: %s", resp.Message)
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
