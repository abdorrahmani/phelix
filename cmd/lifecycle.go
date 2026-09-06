package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/fatih/color"
)

// This file keeps the application lifecycle record (apps.json, AppInfo) in
// agreement with zero-downtime deployment state (deploy.json) and reality
// (live, identity-verified processes).
//
// Root problem being fixed: the blue-green path starts and stops instances
// that the AppManager knows nothing about, so apps.json keeps saying
// "stopped, pid 0" while the proxy happily serves traffic. list/status read
// apps.json and reported the contradiction.

// loadDeployState returns the app's deploy state, or nil when the app is not
// managed by a zero-downtime strategy.
func loadDeployState(appName string) *deploy.DeployState {
	return deploy.LoadZeroDowntime(appName)
}

// servingDeployInstance returns the instance that should be serving traffic
// right now: the active slot for blue-green, or the first recorded replica
// for rolling.
func servingDeployInstance(state *deploy.DeployState) *deploy.Instance {
	switch state.Mode {
	case deploy.ModeBlueGreen:
		return state.ActiveInstance()
	case deploy.ModeRolling:
		return state.ServingInstance()
	}
	return nil
}

// syncAppInfoWithDeploy reconciles AppInfo with deployment reality:
//
//   - every instance the deployment claims to serve is alive → "running",
//     PID = the serving instance's PID (so uptime/RAM/CPU and the list PID
//     column describe the real worker), auto-start intent recorded.
//   - some claimed instances dead, some alive → "degraded".
//   - all claimed instances dead → "stopped", PID 0. deploy.json claiming an
//     active slot is never enough; the process must be alive and resolve to
//     the recorded binary (PID-recycling safe).
//
// The status is derived from verified process liveness here — never from the
// persisted AppInfo/AppInstance status strings, which can be stale. Only
// degraded/running are persisted over the stored record: this read may race a
// concurrent deploy that just wrote fresher state, and stamping "stopped"
// from an old snapshot would clobber it (the list-vs-status contradiction).
func syncAppInfoWithDeploy(info *app.AppInfo, state *deploy.DeployState) error {
	if info == nil || state == nil {
		return nil
	}
	am, ok := app.Manager.(*app.AppManager)
	if !ok {
		return nil
	}

	d, err := state.Reconcile()
	if err != nil {
		return err
	}
	before := *info
	switch d.Status {
	case "running":
		info.Status = "running"
		info.PID = d.PID
		if !d.StartedAt.IsZero() {
			info.Start = d.StartedAt
		}
		info.Port = d.PublicPort
		info.AutoStart = true
	case "degraded":
		info.Status = "degraded"
		info.PID = d.PID // first alive replica, so list shows a real worker
		info.AutoStart = true
	default: // stopped
		info.Status = "stopped"
		info.PID = 0
	}
	if info.Status == before.Status && info.PID == before.PID {
		return nil // nothing drifted — no write, no timestamp churn
	}
	info.UpdatedAt = time.Now()
	return am.SaveState()
}

// reconcileAppWithDeploy is the convenience wrapper used by status/list and
// after deploy operations. `appName` may be an ID or a name — it is resolved
// first, because deploy state is keyed by app name; reconciling with the raw
// identifier made `phelix status <ID>` skip reconciliation entirely and
// disagree with `phelix list`.
func reconcileAppWithDeploy(appName string) {
	if resolved, err := GetAppInfo(appName); err == nil && resolved != nil {
		appName = resolved.Name
	}
	if err := app.Manager.LoadState(); err != nil {
		return
	}
	am, ok := app.Manager.(*app.AppManager)
	if !ok {
		return
	}
	state := loadDeployState(appName)
	if state == nil {
		return
	}
	for _, info := range am.Apps {
		if info.Name == appName {
			_ = syncAppInfoWithDeploy(info, state)
			return
		}
	}
}

// newProxyClientOrNull returns a proxy control client, or nil when the socket
// path cannot be resolved. All uses must tolerate nil.
func newProxyClientOrNull() *proxy.Client {
	socket, err := proxy.DefaultSocketPath()
	if err != nil {
		return nil
	}
	return proxy.NewClient(socket)
}

// runDeployAwareStop tears down a blue-green/rolling deployment for `stop`:
// proxy route removed first (traffic stops flowing), then every instance is
// gracefully stopped, then the lifecycle record is updated. Historical
// metadata in deploy.json is preserved for a later start. The per-app deploy
// lock serialises the teardown against concurrent deploys/rollbacks.
func runDeployAwareStop(appInfo *app.AppInfo) error {
	release, lerr := deploy.AcquireDeployLock(appInfo.Name, "stop")
	if lerr != nil {
		return phelixerr.Wrap(phelixerr.CodeDeployLocked, "could not acquire deploy lock", lerr)
	}
	defer release()

	state := loadDeployState(appInfo.Name)
	if state == nil {
		return phelixerr.Newf(phelixerr.CodeNotFound, "no zero-downtime deployment for %s", appInfo.Name)
	}

	fmt.Printf("%s Stopping deployment for %s (mode %s)\n", color.BlueString("→"), color.CyanString("'%s'", appInfo.Name), state.Mode)

	// 1. Remove the proxy route so traffic stops reaching the instances.
	if client := newProxyClientOrNull(); client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := client.Remove(ctx, appInfo.Name); err != nil {
			fmt.Printf("  %s Could not remove proxy route (daemon down?): %v\n", color.YellowString("⚠"), err)
		} else {
			fmt.Printf("  %s Proxy route removed\n", color.GreenString("✓"))
		}
		cancel()
	}

	// 2. Stop every instance.
	if err := deploy.TeardownDeployment(context.Background(), state, 0, &colorLogger{}); err != nil {
		return err
	}

	// 3. Lifecycle record: stopped, no auto-start.
	am, _ := app.Manager.(*app.AppManager)
	if info := am.Apps[appInfo.ID]; info != nil {
		info.Status = "stopped"
		info.PID = 0
		info.AutoStart = false
		info.UpdatedAt = time.Now()
		if err := am.SaveState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to persist app state", err)
		}
	}
	return nil
}

// runDeployAwareStart restores a stopped blue-green/rolling deployment for
// `start`: proxy daemon ensured, instances relaunched from their recorded
// binaries + env snapshots, proxy route re-established, lifecycle record
// reconciled. The public port stays proxy-owned.
func runDeployAwareStart(appInfo *app.AppInfo) error {
	release, lerr := deploy.AcquireDeployLock(appInfo.Name, "start")
	if lerr != nil {
		return phelixerr.Wrap(phelixerr.CodeDeployLocked, "could not acquire deploy lock", lerr)
	}
	defer release()

	state := loadDeployState(appInfo.Name)
	if state == nil {
		return phelixerr.Newf(phelixerr.CodeNotFound, "no zero-downtime deployment for %s", appInfo.Name)
	}

	// Idempotent: an already-serving deployment is a no-op success.
	if inst := servingDeployInstance(state); inst != nil && deploy.InstanceAlive(inst) {
		if err := syncAppInfoWithDeploy(appInfo, state); err != nil {
			return err
		}
		fmt.Printf("%s Deployment for %s is already running (mode %s)\n",
			color.GreenString("✓"), color.CyanString("'%s'", appInfo.Name), state.Mode)
		return nil
	}

	fmt.Printf("%s Restoring deployment for %s (mode %s)\n", color.BlueString("→"), color.CyanString("'%s'", appInfo.Name), state.Mode)

	ctx := context.Background()
	fmt.Printf("  %s Ensuring proxy daemon is running...\n", color.BlueString("→"))
	if err := proxy.EnsureDaemon(ctx, "", 5*time.Second); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeProxy, err,
			"could not start proxy daemon\n  Start it manually with: %s", color.CyanString("phelix proxy"))
	}

	if err := deploy.StartDeployment(ctx, state, deploy.StartOptions{Logger: &colorLogger{}}); err != nil {
		return err
	}

	client := newProxyClientOrNull()
	if client == nil {
		return phelixerr.New(phelixerr.CodeProxy, "could not connect to proxy control socket")
	}
	if err := deploy.RestoreProxyRoute(ctx, state, client, &colorLogger{}); err != nil {
		return err
	}

	if err := syncAppInfoWithDeploy(appInfo, state); err != nil {
		return err
	}
	fmt.Printf("%s Deployment for %s restored (mode %s, public port %d)\n",
		color.GreenString("✓"), color.CyanString("'%s'", appInfo.Name), state.Mode, state.PublicPort)
	return nil
}

// migrateToClassic performs the documented strategy migration to classic
// (blue-green → classic, rolling → classic): the proxy route is removed, every
// instance is stopped, and deploy.json is deleted so subsequent
// start/stop/rebuild run the classic path. versions.json survives so rollback
// history is kept. A no-op for apps without a zero-downtime deployment.
func migrateToClassic(appName string) error {
	state := loadDeployState(appName)
	if state == nil {
		return nil
	}
	fmt.Printf("%s Migrating %s from %s to classic deployment\n", color.BlueString("→"), color.CyanString("'%s'", appName), state.Mode)
	if err := runDeployAwareStop(&app.AppInfo{Name: appName}); err != nil {
		return err
	}
	return deploy.RemoveState(appName)
}

// stopPublicPortOwner is the PortHandoff implementation for
// classic → blue-green migration: when the classic instance still binds the
// public port, stop it so the proxy can take ownership. It only ever stops
// the app's own managed classic process (via the AppManager, which verifies
// identity); a foreign process holding the port is reported, not killed.
func stopPublicPortOwner(ctx context.Context, appName string, publicPort int) error {
	if err := app.Manager.LoadState(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
	}
	am, _ := app.Manager.(*app.AppManager)
	var info *app.AppInfo
	for _, a := range am.Apps {
		if a.Name == appName {
			info = a
			break
		}
	}
	if info == nil || info.Status != "running" || info.PID <= 0 {
		// Port is held by something Phelix doesn't manage; report it so the
		// user gets an actionable error instead of a cryptic bind failure.
		return phelixerr.Newf(phelixerr.CodePortUnavailable,
			"public port %d is held by an unmanaged process; free it and retry", publicPort)
	}
	fmt.Printf("  %s Stopping classic instance (pid %d) to hand public port %d to the proxy...\n",
		color.BlueString("→"), info.PID, publicPort)
	if err := app.Manager.StopApplication(info.ID); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeProcessFailed, err, "could not stop classic instance of %s", appName)
	}
	return nil
}

// isDeployManagedApp reports whether the identifier resolves to an app that
// is managed by a zero-downtime deployment.
func isDeployManagedApp(identifier string) bool {
	info, err := GetAppInfo(identifier)
	if err != nil {
		return false
	}
	return loadDeployState(info.Name) != nil
}

// deployCmdTimeout bounds proxy-control calls made directly by commands.
const deployCmdTimeout = 5 * time.Second
