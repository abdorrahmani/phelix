package cmd

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/buildreport"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/secrets"
	"github.com/fatih/color"
)

// freshBuildSourceFor builds the FreshBuildSource shared by the zero-downtime
// and worker deploy paths: it compiles via the ordinary rebuildApp pipeline
// and records the version, with lastReport exposed for the build-report emit.
func freshBuildSourceFor(spec *rebuildSpec, out io.Writer, logger *colorLogger) (*deploy.FreshBuildSource, func() *buildreport.Report) {
	buildMgr := spec.BuildMgr
	buildMgr.SetTimeout(spec.Timeout)
	gitCommit := deploy.DetectGitCommit(spec.SourceDir)
	var lastReport *buildreport.Report
	src := &deploy.FreshBuildSource{
		AppName:   spec.Name,
		AppID:     spec.AppInfo.ID,
		ExtraArgs: spec.Args,
		GitCommit: gitCommit,
		Tag:       spec.Tag,
		Retention: deploy.DefaultRetention{Max: 5},
		Logger:    logger,
		ReportFn:  func() *buildreport.Report { return lastReport },
		BuildFn: func(ctx context.Context, appID string, extraArgs []string) (string, error) {
			report, berr := rebuildApp(out, appID, extraArgs, buildMgr, spec.SourceDir)
			lastReport = report
			if berr != nil {
				return "", berr
			}
			if am, ok := app.Manager.(*app.AppManager); ok {
				if info, exists := am.Apps[appID]; exists {
					return filepath.Join(info.Directory, fmt.Sprintf("app_%s", appID)), nil
				}
			}
			return "", phelixerr.Newf(phelixerr.CodeNotFound, "could not locate built binary for %s", appID)
		},
	}
	return src, func() *buildreport.Report { return lastReport }
}

// runWorkerDeploy executes a worker rebuild through the worker deploy engine:
// rolling or classic replica replacement with no proxy, no port and no
// listener checks. The engine reverts replaced replicas on failure, so there
// is no separate auto-rollback step here.
func runWorkerDeploy(spec *rebuildSpec, op *opRun) error {
	appInfo, name := spec.AppInfo, spec.Name
	out := spec.writer()
	logger := &colorLogger{Writer: out}
	cfg := spec.ProjCfg

	replicas := spec.Replicas
	if replicas == 0 && cfg.Deploy != nil {
		replicas = cfg.Deploy.Replicas
	}
	if replicas == 0 {
		if st, lerr := deploy.Load(name); lerr == nil && st != nil && st.Mode == deploy.ModeWorker && len(st.Replicas) > 0 {
			replicas = len(st.Replicas)
		}
	}
	if replicas < 1 {
		replicas = 1
	}
	strategy := spec.Strategy
	if strategy != project.StrategyRolling {
		strategy = project.StrategyClassic
	}

	freshSource, lastReport := freshBuildSourceFor(spec, out, logger)

	tracker := deploy.NewTracker(phelixgrpc.NewDeploymentSink(), appInfo.ID, name, "worker")
	op.setDeploymentID(tracker)
	defer phelixgrpc.StopDeploymentSender(5 * time.Second)

	engine, err := buildWorkerEngine(appInfo, name, cfg, freshSource, replicas, strategy, tracker, logger)
	if err != nil {
		return err
	}

	if err := engine.Deploy(context.Background()); err != nil {
		reconcileAppWithDeploy(name)
		return err
	}
	reconcileAppWithDeploy(name)

	ver := deploy.TargetVersionOf(freshSource)
	gitCommit := deploy.DetectGitCommit(spec.SourceDir)
	fmt.Fprintf(out, "%s Worker deploy complete for %s (%d replicas, strategy %s)\n",
		color.GreenString("✓"), color.CyanString("'%s'", name), replicas, strategy)
	emitBuildReportTo(out, name, lastReport(), gitCommit, &deploy.RecordResult{Version: ver}, nil)
	phelixgrpc.SendVersionListForApp(appInfo.ID, name, appInfo.Directory)
	phelixgrpc.ReportBuildEventForApp(appInfo.ID, name, "classic", true, ver, spec.Tag, gitCommit, lastReport(), "")

	op.setResult(&ops.Result{Version: ver, Strategy: "worker-" + strategy})
	return op.writeResultEnv(machine.Success(op.operationID(), rebuildResult{
		App:      name,
		AppID:    appInfo.ID,
		Version:  ver,
		Strategy: "worker-" + strategy,
	}))
}

// workerGateMode maps the resolved worker health mode onto the deploy engine
// gate: exec keeps its mode, process mode needs no probe.
func workerGateMode(mode string) string {
	if mode == project.WorkerHealthExec {
		return deploy.WorkerGateExec
	}
	return ""
}

// buildWorkerEngine wires the deploy.Worker engine from the project config:
// supervision settings, health gate, rolling drain and the resolved strategy.
// Both the rebuild path and the initial build path use it.
func buildWorkerEngine(appInfo *app.AppInfo, name string, cfg *project.Config, src deploy.BuildSource, replicas int, strategy string, tracker *deploy.Tracker, logger deploy.Logger) (*deploy.Worker, error) {
	ps := cfg.Process.Resolve()
	sig, err := deploy.SignalFromName(ps.ShutdownSignal)
	if err != nil {
		return nil, err
	}
	hs := cfg.Health.ResolveWorker()
	var rollingCfg *project.RollingDeployConfig
	if cfg.Deploy != nil {
		rollingCfg = cfg.Deploy.Rolling
	}
	rs := rollingCfg.Resolve()
	var imports []secrets.Binding
	bound, err := secrets.CommittedBindings(appInfo.ID)
	if err != nil {
		return nil, err
	}
	if cfg.Secrets != nil || len(bound) > 0 {
		imports = []secrets.Binding{}
		if cfg.Secrets != nil {
			imports = append(imports, cfg.Secrets.Imports...)
		}
	}
	return &deploy.Worker{
		SecretImports:  imports,
		AppName:        name,
		AppID:          appInfo.ID,
		Source:         src,
		Strategy:       strategy,
		Replicas:       replicas,
		Dir:            appInfo.Directory,
		Signal:         sig,
		Grace:          ps.GracePeriod,
		DrainTimeout:   rs.DrainTimeout,
		StartupTimeout: ps.StartupTimeout,
		Health: deploy.WorkerGate{
			Mode:    workerGateMode(hs.Mode),
			Command: hs.Command,
			Timeout: hs.Timeout,
		},
		Telemetry: tracker,
		Logger:    logger,
	}, nil
}

// workerReplicaCount resolves the configured worker replica count (1 when
// unset) for status messages.
func workerReplicaCount(cfg *project.Config) int {
	if cfg != nil && cfg.Deploy != nil && cfg.Deploy.Replicas > 0 {
		return cfg.Deploy.Replicas
	}
	return 1
}

// startWorkerInitial launches a worker app's first replica set after an
// initial build: the worker engine runs over the freshly recorded version,
// which creates the ModeWorker state, launches every slot and promotes.
func startWorkerInitial(id, name string, cfg *project.Config, ver int) error {
	appInfo, err := GetAppInfo(id)
	if err != nil {
		return err
	}
	replicas := 1
	strategy := project.StrategyClassic
	if cfg.Deploy != nil {
		if cfg.Deploy.Replicas > 0 {
			replicas = cfg.Deploy.Replicas
		}
		if cfg.Deploy.Strategy == project.StrategyRolling {
			strategy = project.StrategyRolling
		}
	}
	tracker := deploy.NewTracker(phelixgrpc.NewDeploymentSink(), id, name, "worker")
	defer phelixgrpc.StopDeploymentSender(5 * time.Second)
	engine, err := buildWorkerEngine(appInfo, name, cfg,
		&deploy.ExistingVersionSource{AppName: name, Version: ver}, replicas, strategy, tracker,
		&colorLogger{})
	if err != nil {
		return err
	}
	if err := engine.Deploy(context.Background()); err != nil {
		reconcileAppWithDeploy(name)
		return err
	}
	reconcileAppWithDeploy(name)
	return nil
}
