package grpc

import (
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/connstate"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/version"
)

var startTime = time.Now()

// collectMetadata gathers current CLI metadata.
func collectMetadata() *pb.CLIMetadata {
	serverID := server.GetServerID()
	if serverID == "" {
		return nil
	}

	// Count apps
	apps := app.Manager.ListApplications()
	totalApps := len(apps)
	runningApps := 0
	for _, a := range apps {
		if a.Status == "running" {
			runningApps++
		}
	}

	return &pb.CLIMetadata{
		ServerId:         serverID,
		CliVersion:       version.Version,
		BuildVersion:     version.BuildID,
		GitCommit:        version.Commit,
		BuildTime:        version.BuildTime,
		UptimeSeconds:    int64(time.Since(startTime).Seconds()),
		CurrentState:     "running",
		MonitorStatus:    "active",
		TotalManagedApps: int32(totalApps),
		RunningApps:      int32(runningApps),
		Timestamp:        time.Now().UnixMilli(),
		// Backend connection/auth state, separate from the local runtime
		// status above. Older agents leave this empty; the backend must
		// tolerate that.
		ConnectionState: connstate.Get(),
	}
}
