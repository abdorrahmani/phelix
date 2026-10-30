package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/authz"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/ops"
	phelixport "github.com/abdorrahmani/phelix/internal/port"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/workspace"
	"github.com/fatih/color"
)

var (
	// wsDeployAll switches `phelix deploy` into workspace orchestration mode.
	wsDeployAll bool
	// wsDeployJSON requests a machine envelope for the whole run.
	wsDeployJSON bool
	// wsDeployAutoRollback enables per-service automatic rollback on a
	// deploy-phase failure; the mesh still halts, but the failing service
	// keeps its previous version serving.
	wsDeployAutoRollback bool
	// wsDeployTimeout bounds each service's compiler invocation.
	wsDeployTimeout time.Duration
)

type workspaceDeployResult struct {
	Workspace string                       `json:"workspace"`
	Source    string                       `json:"source"`
	Services  []workspaceDeployServiceView `json:"services"`
}

type workspaceDeployServiceView struct {
	Name       string `json:"name"`
	Version    int    `json:"version,omitempty"`
	Port       int    `json:"port,omitempty"`
	Strategy   string `json:"strategy,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// workspaceDeployLog collects one service's progress off the shared stdout so
// parallel deploys cannot interleave their lines.
type workspaceDeployLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *workspaceDeployLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *workspaceDeployLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

var workspaceDeployLogs sync.Map

// workspaceDeployOutput returns the buffered destination for one service.
func workspaceDeployOutput(name string) io.Writer {
	if out, ok := workspaceDeployLogs.Load(name); ok {
		return out.(io.Writer)
	}
	return os.Stdout
}

// workspaceDeployServiceFn is one workspace deploy step: build and deploy a
// single service through the regular rebuild path, returning the promoted
// version, the public port, and the effective strategy.
type workspaceDeployServiceFn func(svc *workspace.Service, info *app.AppInfo) (version, port int, strategy string, err error)

// runWorkspaceDeployAll deploys every service of the workspace in dependency
// order through the regular rebuild path (fresh build, strategy from the
// manifest, per-service deploy lock, health gating, promotion). A failing
// service halts the whole run so downstream services never deploy on top of
// a broken dependency.
func runWorkspaceDeployAll(deployOne workspaceDeployServiceFn) error {
	return orchestrateWorkspaceDeploy(deployOne)
}

// orchestrateWorkspaceDeploy is the injectable core of `phelix deploy --all`;
// tests pass a fake deployOne to exercise ordering and halting without
// compiling anything. Suppressing envelopes for the duration keeps stdout to
// one envelope for the whole run: the per-service rebuild path would
// otherwise write its own.
func orchestrateWorkspaceDeploy(deployOne workspaceDeployServiceFn) error {
	ws, err := workspaceFromCWD()
	if err != nil {
		return err
	}
	if err := workspace.AssignPorts(ws, wsPortBase); err != nil {
		return err
	}
	levels, err := ws.TopoLevels()
	if err != nil {
		return err
	}
	order := flattenWorkspaceLevels(levels)
	if err := authorizeWorkspaceServices(authz.ActionRebuild, order); err != nil {
		return err
	}

	release, err := workspace.AcquireWorkspaceLock(ws.Name, "deploy")
	if err != nil {
		return err
	}
	defer release()

	state, err := loadWorkspaceState(ws)
	if err != nil {
		return err
	}

	printWorkspaceHeader(ws, "deploying")
	restore := suppressEnvelopes()
	defer restore()
	startedAt := time.Now()
	views := make([]workspaceDeployServiceView, 0, len(ws.Services))
	for _, level := range levels {
		levelErr := runParallelLevel(level, wsConcurrency, func(name string) error {
			svc := ws.Service(name)
			info := findRegisteredApp(name)
			if info == nil {
				wsErr := phelixerr.Newf(
					phelixerr.CodeValidation,
					"service %q is not registered yet\nHint: run 'phelix build --all' first",
					name,
				)
				wsPrintMu.Lock()
				state.RecordServiceResult(name, "deploy", "failed", 0, phelixerr.Redact(wsErr.Error()))
				fmt.Printf("  %s %s failed: %v\n", color.RedString("✗"), color.CyanString(name), wsErr)
				wsPrintMu.Unlock()
				return wsErr
			}

			serviceStart := time.Now()
			serviceLog := &workspaceDeployLog{}
			workspaceDeployLogs.Store(name, serviceLog)
			wsPrintMu.Lock()
			fmt.Printf("  %s %s deploying...\n", color.BlueString("→"), color.CyanString(name))
			wsPrintMu.Unlock()
			version, port, strategy, deployErr := deployOne(svc, info)
			workspaceDeployLogs.Delete(name)
			elapsed := time.Since(serviceStart)

			wsPrintMu.Lock()
			defer wsPrintMu.Unlock()
			if deployErr != nil {
				state.RecordServiceResult(name, "deploy", "failed", version, phelixerr.Redact(deployErr.Error()))
				fmt.Printf("  %s %s failed: %v\n", color.RedString("✗"), color.CyanString(name), deployErr)
				if details := serviceLog.String(); details != "" {
					fmt.Print(details)
				}
				return phelixerr.Wrapf(phelixerr.CodeOf(deployErr), deployErr, "workspace deploy halted at service %q", name)
			}
			state.RecordServiceResult(name, "deploy", "succeeded", version, "")
			views = append(views, workspaceDeployServiceView{Name: name, Version: version, Port: port, Strategy: strategy, DurationMS: elapsed.Milliseconds()})
			fmt.Printf("  %s %s %s deployed on :%d (%s, %s)\n",
				color.GreenString("✓"), color.CyanString(name), color.YellowString("v%d", version), port, strategy, elapsed.Round(time.Millisecond))
			return nil
		})
		if levelErr != nil {
			_ = workspace.SaveState(state)
			return levelErr
		}
	}
	if err := workspace.SaveState(state); err != nil {
		return err
	}
	restore()

	fmt.Printf("%s Deploy complete for %s: %d service(s) in %s\n",
		color.GreenString("✓"), color.CyanString(ws.Name), len(ws.Services), time.Since(startedAt).Round(time.Millisecond))
	return writeEnvelopeResult(machine.Success("", workspaceDeployResult{
		Workspace: ws.Name,
		Source:    ws.Source,
		Services:  views,
	}))
}

// flattenWorkspaceLevels turns dependency levels into their execution order.
func flattenWorkspaceLevels(levels [][]string) []string {
	var order []string
	for _, level := range levels {
		order = append(order, level...)
	}
	return order
}

// deployWorkspaceService is the real workspace deploy step. It resolves the
// service's strategy, replicas, and port (manifest first, then the service's
// own phelix.yaml, then its persisted deploy.json), drives the regular
// rebuild execution path, and reports the promoted version.
func deployWorkspaceService(svc *workspace.Service, info *app.AppInfo) (int, int, string, error) {
	projCfg, cfgErr := loadProjectConfigFrom(svc.Path)
	if cfgErr != nil {
		return 0, 0, "", cfgErr
	}
	strategy := resolveWorkspaceStrategy(svc, projCfg, info.Name)
	replicas := resolveWorkspaceReplicas(svc, projCfg, info.Name)

	port := svc.Port
	if port == 0 {
		port = info.Port
	}
	if port == 0 && projCfg != nil {
		port = projCfg.Port
	}
	if port == 0 {
		return 0, 0, strategy, phelixerr.Newf(
			phelixerr.CodeValidation,
			"service %q has no port: set port in %s, phelix.yaml, or build it once so the port persists",
			svc.Name, workspace.FileName,
		)
	}
	if err := phelixport.Validate(port); err != nil {
		return 0, 0, strategy, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "service %q has an invalid port", svc.Name)
	}

	lang := builder.ParseLanguage(info.Language)
	buildMgr := builder.NewBuildManager()
	buildMgr.SetTimeout(wsDeployTimeout)
	if !lang.IsSupported() {
		lang = buildMgr.DetectLanguage(svc.Path)
	}
	if !lang.IsSupported() {
		return 0, 0, strategy, phelixerr.Newf(
			phelixerr.CodeUnsupportedProject,
			"service %q in %s is not a supported project",
			svc.Name, svc.Path,
		)
	}

	deployReplicas := 0
	if strategy == workspace.StrategyRolling {
		deployReplicas = replicas
	}

	spec := &rebuildSpec{
		ProjCfg:      projCfg,
		AppInfo:      info,
		Name:         info.Name,
		Port:         port,
		SourceDir:    svc.Path,
		Strategy:     strategy,
		Replicas:     deployReplicas,
		Lang:         lang,
		BuildMgr:     buildMgr,
		AutoRollback: wsDeployAutoRollback,
		BlueGreen:    strategy == workspace.StrategyBlueGreen,
		Timeout:      wsDeployTimeout,
		Output:       workspaceDeployOutput(svc.Name),
	}
	op := newOpRun(ops.KindRebuild, info.Name, "")
	var runErr error
	defer func() { op.finish(&runErr) }()
	material := map[string]string{
		"strategy": strategy,
		"replicas": fmt.Sprintf("%d", spec.Replicas),
		"port":     fmt.Sprintf("%d", port),
		"source":   svc.Path,
	}
	replayed, idemErr := op.beginIdempotency(material)
	if idemErr != nil {
		return 0, port, strategy, idemErr
	}
	if replayed {
		version, _ := deploy.CurrentVersion(info.Name)
		return version, port, strategy, nil
	}

	runErr = runRebuildExec(spec, op)
	if runErr != nil {
		return 0, port, strategy, runErr
	}
	version, _ := deploy.CurrentVersion(info.Name)
	return version, port, strategy, nil
}

// resolveWorkspaceStrategy layers the manifest over the service's own
// configuration: manifest strategy wins, then phelix.yaml deploy.strategy,
// then the persisted deploy.json mode, then classic.
func resolveWorkspaceStrategy(svc *workspace.Service, projCfg *project.Config, appName string) string {
	strategy := svc.Strategy
	if strategy == "" && projCfg != nil && projCfg.Deploy != nil {
		strategy = projCfg.Deploy.Strategy
	}
	if strategy == "" {
		if state, err := loadDeployState(appName); err == nil && state != nil && state.Mode != "" {
			strategy = string(state.Mode)
		}
	}
	switch strategy {
	case workspace.StrategyBlueGreen, workspace.StrategyRolling:
		return strategy
	case project.StrategyCanary, project.StrategyProgressive:
		return workspace.StrategyClassic
	default:
		return workspace.StrategyClassic
	}
}

// resolveWorkspaceReplicas layers the manifest over phelix.yaml and the
// persisted rolling state. A canary or progressive phelix.yaml strategy
// degrades to classic for workspace deploys, so replicas only follow when
// the resolved strategy is rolling.
func resolveWorkspaceReplicas(svc *workspace.Service, projCfg *project.Config, appName string) int {
	strategy := resolveWorkspaceStrategy(svc, projCfg, appName)
	if strategy != workspace.StrategyRolling {
		return 0
	}
	if svc.Replicas > 0 {
		return svc.Replicas
	}
	if projCfg != nil && projCfg.Deploy != nil && projCfg.Deploy.Replicas > 0 {
		return projCfg.Deploy.Replicas
	}
	if state, err := loadDeployState(appName); err == nil && state != nil && len(state.Replicas) > 0 {
		return len(state.Replicas)
	}
	return 1
}
