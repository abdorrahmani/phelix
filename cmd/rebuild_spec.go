package cmd

// rebuild_spec.go splits `phelix rebuild` into the two halves Phase 3 plans
// must share:
//
//	buildRebuildSpec — resolve everything the execution path WOULD use
//	                   (pure reads; also used by `phelix plan create rebuild`)
//	runRebuildExec   — the existing execution body, driven by that spec
//
// This is the "one validated execution specification" requirement: plans
// capture exactly what buildRebuildSpec resolves, and plan application runs
// runRebuildExec — the same code path `phelix rebuild` runs — never a
// parallel simulator.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixport "github.com/abdorrahmani/phelix/internal/port"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/toolchain"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// rebuildSpec is the resolved execution specification for one rebuild.
type rebuildSpec struct {
	ProjCfg   *project.Config
	AppInfo   *app.AppInfo
	Name      string
	Port      int
	SourceDir string
	// Strategy is the effective strategy resolution produced: classic,
	// blue-green, rolling, canary or progressive.
	Strategy string
	Replicas int
	Canary   int
	Lang     builder.Language
	BuildMgr *builder.BuildManager
}

// buildRebuildSpec resolves a rebuild exactly the way the rebuild command
// does — project config, app identity, isolated source, port precedence and
// deployment strategy — without mutating anything (state convergence like
// syncProjectResources belongs to execution, not resolution). The plan
// subsystem captures this spec; the rebuild command continues from it.
func buildRebuildSpec(cmd *cobra.Command, args []string) (*rebuildSpec, error) {
	projCfg, err := loadProjectConfig()
	if err != nil {
		return nil, err
	}

	if err := app.Manager.LoadState(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
	}

	if len(args) == 0 && projCfg != nil && projCfg.Name != "" {
		if _, err := GetAppInfo(projCfg.Name); err == nil {
			args = []string{projCfg.Name}
		}
	}
	if len(args) == 0 {
		if !IsInteractive() {
			return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <ID|AppName>; usage: phelix rebuild <ID|AppName> --port <PORT>")
		}
		identifier, err := PromptApp(false, "Select application to rebuild")
		if err != nil {
			return nil, err
		}
		args = []string{identifier}
	}

	identifier := args[0]

	appInfo, err := GetAppInfo(identifier)
	if err != nil {
		return nil, err
	}

	buildSource := appInfo.Directory
	if rebuildSourceDir != "" {
		abs, err := filepath.Abs(rebuildSourceDir)
		if err != nil {
			return nil, phelixerr.Wrapf(phelixerr.CodeInvalidArgument, err, "--source-dir %q could not be resolved", rebuildSourceDir)
		}
		if st, serr := os.Stat(abs); serr != nil || !st.IsDir() {
			return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "--source-dir %q is not an existing directory", rebuildSourceDir)
		}
		buildSource = abs

		sourceCfg, serr := loadProjectConfigFrom(buildSource)
		if serr != nil {
			return nil, serr
		}
		if sourceCfg != nil {
			projCfg = sourceCfg
		}
	}

	name, portToUse := DetermineAppParameters(appInfo, cmd, rebuildPort)
	if !cmd.Flags().Changed("port") && (appInfo.Port == 0) && projCfg != nil && projCfg.Port != 0 {
		portToUse = projCfg.Port
	}

	if err := applyConfigDeployStrategy(cmd, projCfg, name); err != nil {
		return nil, err
	}

	if err := phelixport.Validate(portToUse); err != nil {
		return nil, err
	}

	buildMgr := builder.NewBuildManager()
	buildMgr.SetTimeout(rebuildTimeout)
	lang := builder.ParseLanguage(appInfo.Language)
	if !lang.IsSupported() {
		lang = buildMgr.DetectLanguage(buildSource)
	}
	if !lang.IsSupported() {
		return nil, phelixerr.Newf(
			phelixerr.CodeUnsupportedProject,
			"unsupported or unknown project language: %s",
			lang,
		)
	}

	effectiveStrategy := "classic"
	switch {
	case rolloutPlan != nil:
		effectiveStrategy = rolloutPlan.Strategy
	case rebuildBlueGreen:
		effectiveStrategy = "blue-green"
	case rebuildReplicas > 0:
		effectiveStrategy = "rolling"
	}

	return &rebuildSpec{
		ProjCfg:   projCfg,
		AppInfo:   appInfo,
		Name:      name,
		Port:      portToUse,
		SourceDir: buildSource,
		Strategy:  effectiveStrategy,
		Replicas:  rebuildReplicas,
		Canary:    rebuildCanary,
		Lang:      lang,
		BuildMgr:  buildMgr,
	}, nil
}

// runRebuildExec executes a resolved rebuild specification through the
// existing execution body: toolchain, state convergence, and the classic or
// zero-downtime engine dispatch. It is the only execution entry point for
// both `phelix rebuild` and `phelix plan apply`.
func runRebuildExec(spec *rebuildSpec, op *opRun) error {
	appInfo, name, portToUse, buildSource := spec.AppInfo, spec.Name, spec.Port, spec.SourceDir
	buildMgr, lang, projCfg := spec.BuildMgr, spec.Lang, spec.ProjCfg

	resourceCfg, err := loadProjectConfigFrom(buildSource)
	if err != nil {
		return err
	}
	stateChanged, err := syncProjectResources(resourceCfg, appInfo.ID)
	if err != nil {
		return err
	}
	fmt.Printf("  %s Checking toolchain...\n", color.BlueString("→"))
	if err := toolchain.EnsureTool(lang, Confirm); err != nil {
		return phelixerr.Wrap(phelixerr.CodeToolchainNotFound, "toolchain check failed", err)
	}

	fmt.Printf("%s Rebuilding application %s (ID: %s)\n", color.BlueString("→"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID))
	fmt.Printf("  Language: %s\n", color.GreenString(buildMgr.FormatLanguage(lang)))

	if changed, serr := syncProjectWatching(projCfg, appInfo.ID); serr != nil {
		fmt.Printf("  %s Warning: could not apply watching from %s: %v\n",
			color.YellowString("⚠"), project.FileName, serr)
	} else {
		stateChanged = stateChanged || changed
	}

	if stateChanged {
		if err := app.Manager.SaveState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to persist app state", err)
		}
	}

	if serr := syncProjectHealth(projCfg, appInfo.ID, name, portToUse); serr != nil {
		fmt.Printf("  %s Warning: could not apply health endpoints from %s: %v\n",
			color.YellowString("⚠"), project.FileName, serr)
	}

	if rebuildBlueGreen || rebuildReplicas > 0 || rolloutPlan != nil {
		return runZeroDowntimeDeploy(appInfo, name, portToUse, buildSource, op)
	}

	if err := migrateToClassic(name); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeDeployFailed, err, "could not migrate %q to classic deployment", name)
	}

	return runClassicRebuild(spec, op)
}
