package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/fatih/color"
)

// markJobBuilt records that the app is a built job and not a running process.
func markJobBuilt(id string) {
	manager, ok := app.Manager.(*app.AppManager)
	if !ok {
		return
	}
	info, ok := manager.Apps[id]
	if !ok {
		return
	}
	info.Status = "stopped"
	info.BuildStatus = "success"
	info.AutoStart = false
	info.PID = 0
	info.UpdatedAt = time.Now()
	_ = manager.SaveState()
}

// runJobRebuild builds and records a new version for a job, promotes it, and
// never starts a supervised process. The monitor picks the new version up on
// its next scheduled run.
func runJobRebuild(spec *rebuildSpec, op *opRun) error {
	out := spec.writer()
	logger := &colorLogger{Writer: out}
	source, lastReport := freshBuildSourceFor(spec, out, logger)

	if _, _, err := source.Build(context.Background()); err != nil {
		return err
	}
	version := source.TargetVersion()
	if version <= 0 {
		return phelixerr.New(phelixerr.CodeBuildFailed, "job rebuild did not record a version")
	}
	if err := deploy.PromoteVersion(spec.Name, version, "job"); err != nil {
		return err
	}
	markJobBuilt(spec.AppInfo.ID)

	gitCommit := deploy.DetectGitCommit(spec.SourceDir)
	fmt.Fprintf(out, "%s Job %s rebuilt (version v%d); it will run on its schedule\n",
		color.GreenString("✓"), color.CyanString("'%s'", spec.Name), version)

	emitBuildReportTo(out, spec.Name, lastReport(), gitCommit, &deploy.RecordResult{Version: version}, nil)
	phelixgrpc.SendVersionListForApp(spec.AppInfo.ID, spec.Name, spec.AppInfo.Directory)
	phelixgrpc.ReportBuildEventForApp(spec.AppInfo.ID, spec.Name, "job", true, version, spec.Tag, gitCommit, lastReport(), "")

	op.setResult(&ops.Result{Version: version, Strategy: "job"})
	return op.writeResultEnv(machine.Success(op.operationID(), rebuildResult{
		App:      spec.Name,
		AppID:    spec.AppInfo.ID,
		Version:  version,
		Strategy: "job",
	}))
}
