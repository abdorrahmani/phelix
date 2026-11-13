package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/hooks"
	"github.com/abdorrahmani/phelix/internal/project"
)

type hookAdapter struct {
	runner *hooks.Runner
	config *project.HooksConfig
}

func (a *hookAdapter) RunHook(ctx context.Context, req deploy.HookRequest) error {
	if a == nil || a.config == nil {
		return nil
	}
	a.runner.Version = req.Version
	a.runner.PreviousVersion = req.PreviousVersion
	result := a.runner.Run(ctx, req.Name, a.config.Get(req.Name))
	if !result.Aborted() {
		return nil
	}
	if result.Err != nil {
		return result.Err
	}
	return phelixerr.Newf(phelixerr.CodeProcessFailed, "%s hook failed", req.Name)
}

func newHookRunner(appName, appID, projectDir, deploymentID, strategy string, cfg *project.HooksConfig, version, previous int) (*hookAdapter, *hooks.Runner) {
	if cfg == nil {
		return nil, nil
	}
	runner := &hooks.Runner{
		AppName:         appName,
		AppID:           appID,
		ProjectDir:      projectDir,
		DeploymentID:    deploymentID,
		Strategy:        strategy,
		Version:         version,
		PreviousVersion: previous,
		Logger:          &colorLogger{},
		Store:           hooks.NewStore(appName),
	}
	return &hookAdapter{runner: runner, config: cfg}, runner
}

func recoverHookExecutions(appName, deploymentID string) {
	if deploymentID == "" {
		return
	}
	_, _ = hooks.NewStore(appName).Recover(deploymentID)
}

// runBuildHook executes one build lifecycle hook outside a deployment strategy.
func runBuildHook(ctx context.Context, runner *hooks.Runner, cfg *project.HooksConfig, name string, version, previous int) error {
	if runner == nil || cfg == nil {
		return nil
	}
	runner.Version = version
	runner.PreviousVersion = previous
	result := runner.Run(ctx, name, cfg.Get(name))
	if !result.Aborted() {
		return nil
	}
	if result.Err != nil {
		return result.Err
	}
	return phelixerr.Newf(phelixerr.CodeProcessFailed, "%s hook failed", name)
}

func hookConfigFromDir(dir string) (*project.Config, error) {
	if dir == "" {
		return nil, nil
	}
	return loadProjectConfigFrom(dir)
}

func newDeploymentHookRunner(appName, appID, projectDir, deploymentID, strategy string, cfg *project.Config) *hookAdapter {
	if cfg == nil || cfg.Hooks == nil {
		return nil
	}
	adapter, runner := newHookRunner(appName, appID, projectDir, deploymentID, strategy, cfg.Hooks, 0, 0)
	if runner != nil {
		recoverHookExecutions(appName, deploymentID)
	}
	return adapter
}

func printHookSummary(runner *hooks.Runner) {
	if runner == nil {
		return
	}
	execs, err := runner.Store.List()
	if err != nil {
		return
	}
	var count int
	for _, exec := range execs {
		if exec.DeploymentID == runner.DeploymentID {
			count++
		}
	}
	if count > 0 {
		fmt.Printf("  → %d lifecycle hook execution(s) recorded\n", count)
	}
}

func hookDuration(d time.Duration) string {
	return d.Round(time.Millisecond).String()
}

func stopExistingAppWithHooks(ctx context.Context, out io.Writer, appInfo *app.AppInfo, adapter *hookAdapter, cfg *project.Config, version, previous int) error {
	if adapter != nil && cfg != nil {
		if err := runBuildHook(ctx, adapter.runner, cfg.Hooks, project.HookPreStop, version, previous); err != nil {
			return err
		}
	}
	return stopExistingApp(out, appInfo)
}

func hookRunnerOf(adapter *hookAdapter) *hooks.Runner {
	if adapter == nil {
		return nil
	}
	return adapter.runner
}

func hookConfigOf(cfg *project.Config) *project.HooksConfig {
	if cfg == nil {
		return nil
	}
	return cfg.Hooks
}

func runPreStopHook(ctx context.Context, appInfo *app.AppInfo, strategy string) error {
	if appInfo == nil {
		return nil
	}
	cfg, err := hookConfigFromDir(appInfo.Directory)
	if err != nil {
		return err
	}
	if cfg == nil || cfg.Hooks == nil {
		return nil
	}
	version, _ := deploy.CurrentVersion(appInfo.Name)
	deploymentID := fmt.Sprintf("stop-%s-%d", appInfo.ID, time.Now().UnixNano())
	adapter, _ := newHookRunner(appInfo.Name, appInfo.ID, appInfo.Directory, deploymentID, strategy, cfg.Hooks, version, 0)
	return runBuildHook(ctx, hookRunnerOf(adapter), cfg.Hooks, project.HookPreStop, version, 0)
}

func runPostRollbackHook(ctx context.Context, appInfo *app.AppInfo, strategy string, version, previous int, deploymentID string) {
	if appInfo == nil {
		return
	}
	cfg, err := hookConfigFromDir(appInfo.Directory)
	if err != nil || cfg == nil || cfg.Hooks == nil {
		return
	}
	if deploymentID == "" {
		deploymentID = fmt.Sprintf("rollback-%s-%d", appInfo.ID, time.Now().UnixNano())
	}
	adapter, _ := newHookRunner(appInfo.Name, appInfo.ID, appInfo.Directory, deploymentID, strategy, cfg.Hooks, version, previous)
	if err := runBuildHook(ctx, hookRunnerOf(adapter), cfg.Hooks, project.HookPostRollback, version, previous); err != nil {
		(&colorLogger{}).Warnf("post_rollback hook failed; the rollback itself succeeded: %v", err)
	}
}
