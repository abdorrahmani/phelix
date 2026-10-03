package grpc

import (
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
)

// First-class version-history sync. VersionSync carries an app's full
// versions.json (every retained VersionMeta, including the build report) over
// the monitor stream, pushed on reconnect and periodically — the same resync
// idiom as deployment_snapshot. It supersedes the legacy piggy-back of version
// lists on the rollback RPC (SendVersionListForApp), which is kept working for
// an additive cutover: older backends keep reading the rollback path until they
// adopt VersionSync.

// toProtoVersionSync builds the version-history snapshot for one app from
// versions.json. Returns nil when the app has no retained versions.
func toProtoVersionSync(appID, appName string) *pb.VersionSync {
	policy := deploy.DefaultRetention{Max: 5}
	vers, err := deploy.ListVersionsForDisplay(appName, policy)
	if err != nil || len(vers) == 0 {
		return nil
	}
	out := &pb.VersionSync{AppId: appID, AppName: appName}
	for i := range vers {
		v := &vers[i]
		entry := &pb.VersionEntry{
			Version:     int32(v.Version),
			Tag:         v.Tag,
			GitCommit:   v.GitCommit,
			BuiltAt:     unixMilli(v.BuiltAt),
			SizeBytes:   v.SizeBytes,
			IsCurrent:   v.IsCurrent,
			PruneSoon:   deploy.WouldPruneOnNextBuild(appName, v.Version, policy),
			DeployMode:  v.DeployMode,
			DockerImage: v.DockerImage,
			BuildReport: toProtoBuildReport(v.BuildReport),
		}
		if v.DeployedAt != nil {
			entry.DeployedAt = unixMilli(*v.DeployedAt)
		}
		out.Versions = append(out.Versions, entry)
	}
	return out
}

// sendVersionSyncs pushes every watched app's version history over the monitor
// stream. Unwatched apps are skipped at this boundary, matching
// sendDeploymentSnapshots. Failures are logged, never fatal: the next tick (or
// reconnect) retries.
func (c *Client) sendVersionSyncs() {
	for _, a := range app.Manager.ListApplications() {
		if !a.Watching {
			continue
		}
		vs := toProtoVersionSync(a.ID, a.Name)
		if vs == nil {
			continue
		}
		event := &pb.MonitorEvent{
			ServerId:  server.GetServerID(),
			Timestamp: time.Now().UnixMilli(),
			Payload:   &pb.MonitorEvent_VersionSync{VersionSync: vs},
		}
		if err := monitorStream.send(event); err != nil {
			logs.ErrorFile("grpc", "[gRPC Monitor] failed to send version sync for %s: %v", a.Name, err)
			return
		}
	}
}
