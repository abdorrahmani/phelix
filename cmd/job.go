package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/job"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	jobHistoryLimit  int
	jobLogsExecution string
)

var JobCmd = &cobra.Command{
	Use:   "job",
	Short: "Run and inspect scheduled jobs (type: job).",
	Long:  "Scheduled jobs are one-shot commands. The monitor runs them on their cron schedule; these commands list, run and inspect them manually.",
}

var jobListCmd = &cobra.Command{
	Use:   "list",
	Short: "List registered scheduled jobs.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
		}
		jobs, err := registeredJobs()
		if err != nil {
			return err
		}
		if len(jobs) == 0 {
			fmt.Println("No scheduled jobs found.")
			return nil
		}
		for _, j := range jobs {
			fmt.Printf("%s  %s\n", color.CyanString("%s", j.name), color.HiBlackString("(ID: %s)", j.id))
			fmt.Printf("  schedule: %s (%s)\n", j.schedule.Cron, j.location)
			fmt.Printf("  command:  %s\n", strings.Join(j.settings.Command, " "))
		}
		return nil
	},
}

var jobRunCmd = &cobra.Command{
	Use:   "run <NAME>",
	Short: "Run a scheduled job once, now.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
		}
		info, cfg, err := jobTarget(args[0])
		if err != nil {
			return err
		}
		settings := cfg.Job.Resolve()
		executor := job.NewExecutor(job.NewStore(server.DataDir()))
		executor.Log = jobWarn
		record, err := executor.Run(cmd.Context(), job.RunRequest{
			AppName:     info.Name,
			AppID:       info.ID,
			Dir:         info.Directory,
			Command:     settings.Command,
			Timeout:     settings.Timeout,
			Retries:     settings.Retries,
			Concurrency: settings.ConcurrencyPolicy,
			Trigger:     job.TriggerManual,
			Version:     currentJobVersion(info.Name),
		})
		if err != nil {
			return err
		}
		fmt.Printf("%s %s: %s (execution %s)\n", statusMark(record.Status),
			color.CyanString("'%s'", info.Name), statusText(record.Status), color.YellowString(record.ID))
		if record.Status != job.StatusSucceeded {
			return phelixerr.Newf(phelixerr.CodeProcessFailed, "job %q finished with status %s", info.Name, record.Status)
		}
		return nil
	},
}

var jobHistoryCmd = &cobra.Command{
	Use:   "history <NAME>",
	Short: "Show recent executions of a scheduled job.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
		}
		info, _, err := jobTarget(args[0])
		if err != nil {
			return err
		}
		records, err := job.NewStore(server.DataDir()).List(info.Name, jobHistoryLimit)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			fmt.Printf("No executions for job %q.\n", info.Name)
			return nil
		}
		for _, record := range records {
			fmt.Printf("%s  %s  %-10s  %s  exit=%s  %s\n",
				color.YellowString(record.ID),
				record.StartedAt.Local().Format("2006-01-02 15:04:05"),
				record.Status,
				formatDuration(record.DurationMS),
				formatExitCode(record.Status, record.ExitCode),
				record.Trigger,
			)
		}
		return nil
	},
}

var jobLogsCmd = &cobra.Command{
	Use:   "logs <NAME>",
	Short: "Print the output log of a job execution.",
	Long:  "Prints the log of the given execution, or of the newest execution when --execution is omitted.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
		}
		info, _, err := jobTarget(args[0])
		if err != nil {
			return err
		}
		store := job.NewStore(server.DataDir())
		id := jobLogsExecution
		if id == "" {
			records, err := store.List(info.Name, 1)
			if err != nil {
				return err
			}
			if len(records) == 0 {
				return phelixerr.Newf(phelixerr.CodeNotFound, "job %q has no executions", info.Name)
			}
			id = records[0].ID
		}
		if _, err := store.Load(info.Name, id); err != nil {
			return err
		}
		path, err := store.LogPath(info.Name, id)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return phelixerr.Newf(phelixerr.CodeNotFound, "execution %s has no log output", id)
		}
		if err != nil {
			return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "job: read log %s", path)
		}
		_, err = os.Stdout.Write(data)
		return err
	},
}

func init() {
	jobHistoryCmd.Flags().IntVar(&jobHistoryLimit, "limit", 20, "Maximum number of executions to show")
	jobLogsCmd.Flags().StringVar(&jobLogsExecution, "execution", "", "Execution ID to print (default: newest)")
	JobCmd.AddCommand(jobListCmd, jobRunCmd, jobHistoryCmd, jobLogsCmd)
}

type registeredJob struct {
	name     string
	id       string
	schedule project.ScheduleSettings
	location string
	settings project.JobSettings
}

// registeredJobs returns every app declaring type: job.
func registeredJobs() ([]registeredJob, error) {
	var out []registeredJob
	for _, info := range app.Manager.ListApplications() {
		if info.Directory == "" {
			continue
		}
		cfg, err := project.Load(info.Directory)
		if err != nil {
			fmt.Printf("  %s Skipping %s: %v\n", color.YellowString("⚠"), info.Name, err)
			continue
		}
		if cfg == nil || !cfg.IsJob() {
			continue
		}
		schedule := cfg.Schedule.Resolve()
		out = append(out, registeredJob{
			name:     info.Name,
			id:       info.ID,
			schedule: schedule,
			location: schedule.Location.String(),
			settings: cfg.Job.Resolve(),
		})
	}
	return out, nil
}

// jobTarget resolves an identifier to a job app and its configuration.
func jobTarget(identifier string) (*app.AppInfo, *project.Config, error) {
	info, err := GetAppInfo(identifier)
	if err != nil {
		return nil, nil, err
	}
	if err := deploy.ValidateAppName(info.Name); err != nil {
		return nil, nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "invalid job name %q", info.Name)
	}
	cfg, err := project.Load(info.Directory)
	if err != nil {
		return nil, nil, err
	}
	if cfg == nil || !cfg.IsJob() {
		return nil, nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%q is not a scheduled job\nHint: declare type: job in %s", info.Name, project.FileName)
	}
	if len(cfg.Job.Resolve().Command) == 0 {
		return nil, nil, phelixerr.Newf(phelixerr.CodeConfiguration, "job %q has no command", info.Name)
	}
	return info, cfg, nil
}

func currentJobVersion(name string) int {
	version, err := deploy.CurrentVersion(name)
	if err != nil {
		return 0
	}
	return version
}

func jobWarn(level, format string, args ...any) {
	if level == "warning" {
		fmt.Fprintf(os.Stderr, "warning: "+format+"\n", args...)
	}
}

func formatDuration(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return (time.Duration(ms) * time.Millisecond).String()
}

func formatExitCode(status job.Status, code int) string {
	if status == job.StatusRunning || status == job.StatusPending {
		return "-"
	}
	return strconv.Itoa(code)
}

func statusMark(status job.Status) string {
	switch status {
	case job.StatusSucceeded:
		return color.GreenString("✓")
	case job.StatusRunning, job.StatusPending:
		return color.BlueString("→")
	case job.StatusSkipped:
		return color.YellowString("-")
	default:
		return color.RedString("✗")
	}
}

func statusText(status job.Status) string {
	return string(status)
}
