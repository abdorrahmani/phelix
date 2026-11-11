package cmd

import (
	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/project"
)

// isJobDir reports whether the project directory declares type: job.
func isJobDir(dir string) bool {
	if dir == "" {
		return false
	}
	cfg, err := project.Load(dir)
	return err == nil && cfg != nil && cfg.IsJob()
}

// rejectJobLifecycle keeps service and worker commands from treating a job as
// a long-running process.
func rejectJobLifecycle(info *app.AppInfo, action string) error {
	if info == nil || !isJobDir(info.Directory) {
		return nil
	}
	return phelixerr.Newf(phelixerr.CodeInvalidArgument,
		"cannot %s job %q: a job is one-shot and is not a supervised process\nHint: run it with 'phelix job run %s'", action, info.Name, info.Name)
}
