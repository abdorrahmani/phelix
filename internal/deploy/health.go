package deploy

import (
	"context"
	"fmt"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
)

// deployResolver returns the health resolver a deploy uses to check a
// candidate. A docker instance gets a container-state liveness checker (docker
// inspect .State.Running): a host-PID signal check is wrong across the
// container's uid/namespace boundary — the container's non-root user owns the
// PID (signal 0 → EPERM) and the PID may not even be on the host — which made
// every non-root container look "exited before becoming healthy". A native
// instance gets nil, i.e. the default (now EPERM-safe) host-PID checker. Shared
// by blue-green, rolling, and rollout so all three agree.
func deployResolver(proc Process) *health.Resolver {
	if alive := dockerLivenessChecker(proc); alive != nil {
		return health.NewResolver(health.WithPidChecker(alive))
	}
	return nil
}

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
//
// The probe target reflects what was actually checked: the candidate's own
// internal host:port (never the public proxy port — traffic must not reach the
// candidate before it passes health), the configured health path for Tier 1,
// and the selected tier. The output tail comes from `docker logs` for a docker
// instance and from the on-disk instance log for a native one; the native path
// is byte-for-byte unchanged.
func candidateHealthFailure(proc Process, binaryPath string, port int, tier health.Tier, cfg *health.DeployTierConfig, err error) error {
	detail := "probe target: " + probeTarget(tier, cfg, port)
	if tail := candidateLogTail(proc, binaryPath, port, 2048); tail != "" {
		detail += "\ninstance output (tail):\n" + tail
	}
	return phelixerr.Wrapf(phelixerr.CodeHealthCheckFailed, err, "%s", detail)
}

// probeTarget renders what the health checker actually dialed, so the reported
// target matches the real probe (health.Check builds the URL the same way): an
// HTTP URL with the configured path for Tier 1, HTTP "/" for Tier 2, and a bare
// host:port for the TCP/PID-only tiers. The selected tier is appended so an
// operator sees both where and how the candidate was checked.
func probeTarget(tier health.Tier, cfg *health.DeployTierConfig, port int) string {
	switch tier {
	case health.Tier1HTTPPath:
		path := "/"
		if cfg != nil && cfg.Path != "" {
			path = cfg.Path
		}
		return fmt.Sprintf("http://%s%s (%s)", hostPort(port), path, tier)
	case health.Tier2HTTPAny:
		return fmt.Sprintf("http://%s/ (%s)", hostPort(port), tier)
	default:
		return fmt.Sprintf("%s (%s)", hostPort(port), tier)
	}
}

// candidateLogTail returns the failed candidate's recent output: `docker logs`
// for a docker instance, otherwise the on-disk instance-log tail. A docker
// instance never falls through to the native reader (its binaryPath is an image
// ref, so no such log exists), and a native process never reaches the docker
// reader — so each runtime reports exactly its own output.
func candidateLogTail(proc Process, binaryPath string, port int, maxBytes int64) string {
	if tail := dockerLogTailOf(proc, maxBytes); tail != "" {
		return tail
	}
	return instanceLogTail(binaryPath, port, maxBytes)
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
