package grpc

import (
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
)

// healthSnapshotInterval is how often the monitor daemon re-pushes the health
// state of every app with health checks configured. It matches the default
// endpoint check interval (10s): pushing on the 2s metrics tick would resend the
// same observation five times. It is a var so tests can shorten it.
var healthSnapshotInterval = 10 * time.Second

// sendHealthSnapshots pushes the health state of every watched app over the
// monitor stream.
//
// This is the only agent -> backend health synchronization path. Each message is a
// complete snapshot of one app, so it doubles as the create, update, delete and
// reconnect-recovery mechanism: the backend replaces what it holds for that app.
// Apps with no health configuration produce no snapshot — absence means "health is
// not configured", so nothing is sent for them and reporting begins by itself once
// a configuration is added. The same holds for unwatched apps (Watching=false):
// they are skipped here, so the backend receives no health data for them at all.
// Failures are logged, never fatal: the next tick retries, and a reconnect
// re-sends everything.
func (c *Client) sendHealthSnapshots() {
	daemon := health.RunningDaemon()
	if daemon == nil {
		// No health daemon in this process (a plain CLI command), or it has not
		// started yet. Nothing to report.
		return
	}

	for _, a := range app.Manager.ListApplications() {
		// Watching gate, applied at the reporting boundary so it also holds
		// after a reconnect: an unwatched app sends no health snapshot, even
		// if it has health checks configured (they keep running locally).
		if !a.Watching {
			continue
		}
		snap := daemon.SnapshotForApp(a.ID, a.Name)
		if snap == nil {
			continue
		}
		event := &pb.MonitorEvent{
			ServerId:  server.GetServerID(),
			Timestamp: time.Now().UnixMilli(),
			Payload: &pb.MonitorEvent_AppHealth{
				AppHealth: toProtoAppHealthSnapshot(snap),
			},
		}
		if err := monitorStream.send(event); err != nil {
			logs.ErrorFile("grpc", "[gRPC Monitor] failed to send health snapshot for %s: %v", a.Name, err)
			return
		}
	}
}

// toProtoAppHealthSnapshot converts a health.AppHealthSnapshot to its protobuf
// representation. app_id is the application UUID from apps.json, the same
// identity every other agent->backend message uses; endpoint_id is the stable
// per-endpoint identity from health.json.
func toProtoAppHealthSnapshot(s *health.AppHealthSnapshot) *pb.AppHealthSnapshot {
	out := &pb.AppHealthSnapshot{
		ServerId:       server.GetServerID(),
		AppId:          s.AppID,
		AppName:        s.AppName,
		Enabled:        s.Enabled,
		Status:         s.Status,
		UpdatedAt:      unixMilli(s.UpdatedAt),
		EndpointsTotal: int32(s.EndpointsTotal),
		EndpointsUp:    int32(s.EndpointsUp),
		// Always non-nil, so an app with zero configured endpoints is an
		// explicit empty set the backend can reconcile against rather than an
		// omitted field it has to guess about.
		Endpoints: make([]*pb.AppHealthEndpoint, 0, len(s.Endpoints)),
	}

	if s.DeployTier != nil {
		out.DeployTier = &pb.DeployTierConfigProto{
			Mode:     string(s.DeployTier.Mode),
			Path:     s.DeployTier.Path,
			Interval: s.DeployTier.Interval,
			Retries:  int32(s.DeployTier.Retries),
			Timeout:  s.DeployTier.Timeout,
		}
	}

	for _, e := range s.Endpoints {
		endpoint := &pb.AppHealthEndpoint{
			EndpointId: e.Config.ID,
			Config: &pb.HealthEndpointConfig{
				Name:          e.Config.Name,
				Url:           e.Config.URL,
				Interval:      e.Config.Interval,
				Retries:       int32(e.Config.Retries),
				ExpectedCodes: e.Config.ExpectedCodes,
				Timeout:       e.Config.Timeout,
			},
		}
		// A nil result means the endpoint has not been probed yet; the status
		// message stays absent so the backend can tell that apart from a
		// failure. The runtime counters live on the status message too, so an
		// unprobed endpoint reports no runtime data at all.
		if r := e.Result; r != nil {
			status := &pb.HealthEndpointStatus{
				Status:              r.Status,
				CheckedAt:           unixMilli(r.CheckedAt),
				ConsecutiveFailures: int32(e.ConsecutiveFailures),
				LastSuccessAt:       unixMilli(e.LastSuccess),
				LastFailureAt:       unixMilli(e.LastFailure),
				BackoffLevel:        int32(e.BackoffLevel),
				NextRestartAt:       unixMilli(e.NextRestart),
				CrashCount_24H:      int32(e.CrashCount24h),
			}
			if r.StatusCode != nil {
				status.StatusCode = int32(*r.StatusCode)
			}
			// A timed-out probe has no latency to report; the flag keeps the
			// backend from reading the zero value as "0 ms".
			if r.LatencyMs != nil {
				status.LatencyMs = *r.LatencyMs
				status.LatencyMeasured = true
			}
			if r.Error != nil {
				// Probe errors quote the URL and whatever the transport said, so
				// they are the one free-text field in a snapshot that can pick up
				// a credential. Redacted here, at the boundary, for the same
				// reason cmd redacts before printing.
				status.Error = phelixerr.Redact(*r.Error)
			}
			endpoint.Status = status
		}
		out.Endpoints = append(out.Endpoints, endpoint)
	}

	return out
}
