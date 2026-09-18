package deploy

import (
	"context"
	"fmt"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
)

// selectDeployHealth is the single tier-resolution point shared by blue-green
// and rolling deploys: resolve the effective tier config, select the tier,
// log the selection, and warn (terminal + panel) whenever the deploy is not
// backed by an explicit Tier 1 endpoint. Both modes must agree on tier and
// warning behavior for the same configuration — the warning and selection
// logic used to be duplicated (and could drift) in each deploy flow.
//
// cfg comes from the app's HealthConfigProvider (DefaultHealthProvider reads
// the persisted health configuration `phelix health list/status` shows).
// portAddr is the host:port used for Tier 2/3 auto-detection.
func selectDeployHealth(ctx context.Context, cfg *health.DeployTierConfig, appName, portAddr string, log Logger, notifier Notifier) health.Tier {
	tier := health.SelectTier(cfg, portAddr, nil)
	log.Infof("health tier selected: %s", tier)

	if tier != health.Tier1HTTPPath {
		msg := fmt.Sprintf("⚠ No health endpoint configured for %s — using %s.\n"+
			"  Add one with: phelix health set %s --path /your-health-path",
			appName, tier, appName)
		log.Warnf("%s", msg)
		if notifier != nil {
			_ = notifier.Notify(ctx, msg)
		}
	}
	return tier
}

// candidateHealthFailure enriches a failed candidate health check with the
// exact probe target and, when the candidate died at boot, the tail of its
// captured output — the difference between "app ignores $PORT and panicked
// with AddrInUse" and an opaque 30s timeout. Shared by blue-green and rolling.
// The path and health-check configuration stay as configured; the host:port is
// always the candidate's own internal listener, never the public proxy port
// (traffic must not be routed to the candidate before it passes health).
func candidateHealthFailure(binaryPath string, port int, err error) error {
	detail := fmt.Sprintf("probe target: http://%s", hostPort(port))
	if tail := instanceLogTail(binaryPath, port, 2048); tail != "" {
		detail += "\ninstance output (tail):\n" + tail
	}
	return phelixerr.Wrapf(phelixerr.CodeHealthCheckFailed, err, "%s", detail)
}

// oomFailure upgrades a candidate/replica deployment failure to the
// resource-OOM code when the failed instance provably hit its cgroup memory
// limit: its process handle reports RESOURCE_OOM for the exit (memory.events
// oom_kill increased during the instance's lifetime). The original failure —
// health-check details, probe target, log tail — stays in the chain; only the
// machine-readable code and the headline change, so automation can tell a
// memory-limit kill from an application bug. Call only after the instance was
// stopped and reaped, so proc.Wait never blocks.
func oomFailure(proc Process, failure error, oomHeadline string) error {
	if waitErr := proc.Wait(); phelixerr.IsCode(waitErr, phelixerr.CodeResourceOOM) {
		return phelixerr.Wrapf(phelixerr.CodeResourceOOM, failure, "%s", oomHeadline)
	}
	return failure
}
