package cmd

import (
	"context"
	"sync"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/job"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/server"
)

// runJobScheduler drives scheduled jobs inside the monitor daemon.
func runJobScheduler(ctx context.Context) {
	store := job.NewStore(server.DataDir())
	executor := job.NewExecutor(store)
	executor.Log = func(level, format string, args ...any) {
		if level == "warning" {
			logs.Warning("job", format, args...)
			return
		}
		logs.Info("job", format, args...)
	}
	scheduler := job.NewScheduler(executor)
	scheduler.ListJobs = listScheduledJobs
	scheduler.Log = executor.Log
	scheduler.Run(ctx)
}

// warnedJobConfigs keeps one warning per app and failure, so a monitor polling
// every second never repeats the same broken configuration forever.
var warnedJobConfigs sync.Map

// listScheduledJobs reads every registered app's phelix.yaml and returns the
// ones declaring type: job. Apps whose project file cannot be read are skipped
// with one warning instead of failing the whole schedule.
func listScheduledJobs() []job.ScheduledJob {
	apps := app.Manager.ListApplications()
	out := make([]job.ScheduledJob, 0)
	for _, info := range apps {
		if info.Directory == "" {
			continue
		}
		cfg, err := project.Load(info.Directory)
		if err != nil {
			key := info.Name + ":" + err.Error()
			if _, loaded := warnedJobConfigs.LoadOrStore(key, true); !loaded {
				logs.Warning("job", "cannot load job configuration for %s: %v", info.Name, err)
			}
			continue
		}
		if cfg == nil || !cfg.IsJob() {
			continue
		}
		schedule := cfg.Schedule.Resolve()
		settings := cfg.Job.Resolve()
		out = append(out, job.ScheduledJob{
			AppName:     info.Name,
			AppID:       info.ID,
			Dir:         info.Directory,
			Cron:        schedule.Cron,
			Location:    schedule.Location,
			Command:     settings.Command,
			Timeout:     settings.Timeout,
			Retries:     settings.Retries,
			Concurrency: settings.ConcurrencyPolicy,
		})
	}
	return out
}
