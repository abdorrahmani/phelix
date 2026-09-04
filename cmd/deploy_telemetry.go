package cmd

import (
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
)

// Deployment telemetry for the classic (stop → build → start) path. Blue-green
// and rolling deploys are instrumented inside the deploy package, where their
// slot/replica/proxy transitions live; classic has no such flow — it is a
// single instance on the public port — so its (short) lifecycle is reported
// from here.
//
// A nil tracker means telemetry is off (no session): every call below is then a
// no-op, and the classic rebuild behaves exactly as it did before.

// classicTracker starts telemetry for a classic deployment of app. currentVer
// is the version serving before this deployment (0 when unknown). The returned
// flush must be deferred by the caller so queued events reach the backend
// before the CLI exits.
func classicTracker(appID, appName string, publicPort, currentVer int) (*deploy.Tracker, func()) {
	t := deploy.NewTracker(phelixgrpc.NewDeploymentSink(), appID, appName, deploy.StrategyClassic)
	if t == nil {
		return nil, func() {}
	}
	// Classic serves the public port directly. Saying so explicitly is what
	// lets the backend tell "not proxied" apart from "proxy state unknown".
	t.SetUnproxied(publicPort)
	t.Started(currentVer, 0, "classic deploy of "+appName)
	return t, func() { phelixgrpc.StopDeploymentSender(5 * time.Second) }
}

// classicPID reads the PID the AppManager recorded for a just-started classic
// instance. It re-resolves from the manager rather than reusing an AppInfo
// pointer captured earlier, because LoadState rebuilds the Apps map and an
// older pointer would still report the pre-start PID.
func classicPID(appID string) int {
	info, err := GetAppInfo(appID)
	if err != nil || info == nil {
		return 0
	}
	return info.PID
}
