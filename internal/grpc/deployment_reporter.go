package grpc

import (
	"context"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Deployment telemetry transport. It carries deploy.Event to the backend over
// ReportDeploymentEvent, and DeploymentSnapshot over the monitor stream for
// resync.
//
// Reliability model, matching the rollback reporter this file sits next to: the
// deployment flow never blocks on the network. Events go onto a buffered
// channel drained by one background goroutine that owns a connection; if the
// channel is full or the backend is unreachable, the event is dropped from the
// live feed and phelix.log records it. Nothing is lost permanently, because
// every event carries the full snapshot and the monitor daemon re-pushes the
// current snapshot after each reconnect — the backend converges on real state
// even when it missed the transitions.

// deploymentEventBuffer is how many events may queue before the sender drops.
// A blue-green deploy emits under a dozen events and a rolling deploy a few per
// replica, so this covers a large rollout with headroom.
const deploymentEventBuffer = 128

// The deployment telemetry compiled into this build (DeploymentEvent
// reporting and DeploymentSnapshot resync) declares its capability.
func init() { RegisterCapability(CapabilityDeployment) }

var (
	deploymentEventCh   chan *pb.DeploymentEvent
	deploymentOnce      sync.Once
	deploymentSenderMu  sync.Mutex
	deploymentDone      chan struct{}
	deploymentUnsupport bool // backend answered UNIMPLEMENTED; stop trying
)

// DeploymentReporter is the deploy.Sink implementation that forwards deployment
// telemetry to the backend.
type DeploymentReporter struct{}

// NewDeploymentSink returns a sink for deployment telemetry, or nil when the
// CLI has no session. A nil sink makes deploy.NewTracker return a nil Tracker,
// so an offline or logged-out deploy runs exactly as it did before telemetry
// existed.
func NewDeploymentSink() deploy.Sink {
	if !sessionAvailable() {
		return nil
	}
	return &DeploymentReporter{}
}

// Deployment queues one deployment event for delivery. Never blocks.
func (r *DeploymentReporter) Deployment(ev deploy.Event) {
	enqueueDeploymentEvent(toProtoDeploymentEvent(ev))
}

func initDeploymentSender() {
	deploymentSenderMu.Lock()
	defer deploymentSenderMu.Unlock()

	deploymentOnce.Do(func() {
		deploymentEventCh = make(chan *pb.DeploymentEvent, deploymentEventBuffer)
		deploymentDone = make(chan struct{})
		go deploymentSenderLoop()
		logs.InfoFile("grpc", "[gRPC] Deployment telemetry: background worker started")
	})
}

// deploymentSenderLoop drains queued events, reusing one connection for a batch
// so a rollout does not pay a dial per event.
func deploymentSenderLoop() {
	defer close(deploymentDone)

	var (
		client    *Client
		sent      int
		dropped   int
		ensureCli = func() *Client {
			if client != nil && client.IsConnected() {
				return client
			}
			if client != nil {
				client.Close()
				client = nil
			}
			c := NewClient()
			if err := c.Connect(); err != nil {
				logs.ErrorFile("grpc", "[gRPC] Deployment telemetry: connect failed: %v", err)
				return nil
			}
			client = c
			return client
		}
	)

	for ev := range deploymentEventCh {
		if deploymentUnsupported() {
			dropped++
			continue
		}
		c := ensureCli()
		if c == nil {
			dropped++
			logs.WarningFile("grpc", "[gRPC] Deployment telemetry: no connection, dropping %s (dropped=%d)", ev.GetEvent(), dropped)
			continue
		}
		if err := c.SendDeploymentEvent(ev); err != nil {
			dropped++
			logs.ErrorFile("grpc", "[gRPC] Deployment telemetry: send failed for %s: %v (dropped=%d)", ev.GetEvent(), err, dropped)
			// A dead connection must not poison the rest of the batch.
			if client != nil {
				client.Close()
				client = nil
			}
			continue
		}
		sent++
	}

	if client != nil {
		client.Close()
	}
	logs.InfoFile("grpc", "[gRPC] Deployment telemetry: stopped (sent=%d, dropped=%d)", sent, dropped)
}

// enqueueDeploymentEvent hands the event to the background sender. Returns
// false when the buffer is full (event dropped from the live feed; the backend
// still converges via snapshot resync).
func enqueueDeploymentEvent(ev *pb.DeploymentEvent) bool {
	if ev == nil {
		return false
	}
	initDeploymentSender()

	// Read the channel under the same mutex StopDeploymentSender uses to
	// replace it, so an event emitted concurrently with a flush can never be
	// sent on a closed channel.
	deploymentSenderMu.Lock()
	ch := deploymentEventCh
	deploymentSenderMu.Unlock()
	if ch == nil {
		return false
	}

	select {
	case ch <- ev:
		return true
	default:
		logs.WarningFile("grpc", "[gRPC] Deployment telemetry: queue full, dropped %s for %s", ev.GetEvent(), ev.GetAppName())
		return false
	}
}

// StopDeploymentSender drains queued deployment events and waits for the
// background sender to finish, so a short-lived CLI command does not exit with
// its deployment's last events still in the queue. Safe when no sender ran.
func StopDeploymentSender(timeout time.Duration) {
	deploymentSenderMu.Lock()
	ch := deploymentEventCh
	done := deploymentDone
	if ch == nil || done == nil {
		deploymentSenderMu.Unlock()
		return
	}
	close(ch)
	deploymentEventCh = nil
	deploymentDone = nil
	deploymentOnce = sync.Once{}
	deploymentSenderMu.Unlock()

	select {
	case <-done:
	case <-time.After(timeout):
		logs.WarningFile("grpc", "[gRPC] Deployment telemetry: flush timed out after %s", timeout)
	}
}

// markDeploymentUnsupported records that the backend does not implement
// ReportDeploymentEvent, so the CLI stops attempting it for this process.
func markDeploymentUnsupported() {
	deploymentSenderMu.Lock()
	if !deploymentUnsupport {
		deploymentUnsupport = true
		logs.WarningFile("grpc", "[gRPC] Deployment telemetry: backend does not implement ReportDeploymentEvent; skipping for this run")
	}
	deploymentSenderMu.Unlock()
}

func deploymentUnsupported() bool {
	deploymentSenderMu.Lock()
	defer deploymentSenderMu.Unlock()
	return deploymentUnsupport
}

// SendDeploymentEvent delivers one deployment event. An UNIMPLEMENTED reply
// from an older backend is not an error worth retrying: it disables deployment
// telemetry for the rest of the process and returns nil so the caller does not
// treat it as a delivery failure.
func (c *Client) SendDeploymentEvent(ev *pb.DeploymentEvent) error {
	if !c.IsConnected() {
		return phelixerr.New(phelixerr.CodeConnection, "deployment telemetry: not connected")
	}
	svc := c.GetServiceClient()
	if svc == nil {
		return phelixerr.New(phelixerr.CodeConnection, "deployment telemetry: no service client")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	authCtx, err := attachAuthMetadata(ctx, server.GetServerID())
	if err != nil {
		return err
	}

	resp, err := svc.ReportDeploymentEvent(authCtx, ev)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			markDeploymentUnsupported()
			return nil
		}
		c.reconnectIfNeeded()
		return err
	}
	if !resp.GetAccepted() {
		logs.WarningFile("grpc", "[gRPC] Deployment event rejected: %s", resp.GetMessage())
	}
	return nil
}

// --- snapshot resync -------------------------------------------------------

// deploymentResyncInterval is how often the monitor daemon re-pushes every
// app's deployment snapshot. Deployment topology only changes during a deploy
// (which reports its own events), so this is a slow safety net against
// divergence rather than a metrics feed.
var deploymentResyncInterval = 60 * time.Second

// sendDeploymentSnapshots pushes the current deployment topology of every
// watched app over the monitor stream. Apps with no zero-downtime deployment
// produce no snapshot. Unwatched apps (Watching=false) are skipped at this
// reporting boundary — including after a reconnect — so the backend receives
// no deployment topology for them. Failures are logged, never fatal: the next
// tick retries.
func (c *Client) sendDeploymentSnapshots() {
	pc := deployProxyClient()
	for _, a := range app.Manager.ListApplications() {
		if !a.Watching {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		snap := deploy.SnapshotForApp(ctx, a.Name, a.ID, pc)
		cancel()
		if snap == nil {
			continue
		}
		event := &pb.MonitorEvent{
			ServerId:  server.GetServerID(),
			Timestamp: time.Now().UnixMilli(),
			Payload: &pb.MonitorEvent_DeploymentSnapshot{
				DeploymentSnapshot: ToProtoDeploymentSnapshot(snap),
			},
		}
		if err := monitorStream.send(event); err != nil {
			logs.ErrorFile("grpc", "[gRPC Monitor] failed to send deployment snapshot for %s: %v", a.Name, err)
			return
		}
	}
}

// deployProxyClient returns a proxy control client so snapshots report the
// daemon's real routing. nil when the socket path cannot be resolved, in which
// case the snapshot omits proxy state rather than guessing it.
func deployProxyClient() deploy.ProxyClient {
	socket, err := proxy.DefaultSocketPath()
	if err != nil {
		return nil
	}
	return proxy.NewClient(socket)
}
