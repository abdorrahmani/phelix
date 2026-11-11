package project

import (
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/robfig/cron/v3"
)

// TypeJob is the workload type for a one-shot command run on a cron schedule.
const TypeJob = "job"

// Missed policy values for schedule.missed. v1 only supports skipping.
const MissedSkip = "skip"

// Concurrency policies for job.concurrency.policy.
const (
	ConcurrencyForbid = "forbid"
	ConcurrencyAllow  = "allow"
)

// Defaults applied by the job Resolve methods for fields left unset.
const (
	DefaultJobScheduleTimezone = "UTC"
	DefaultJobTimeout          = 30 * time.Minute
	DefaultJobConcurrency      = ConcurrencyForbid
)

var supportedJobConcurrencyPolicies = map[string]bool{
	ConcurrencyForbid: true, ConcurrencyAllow: true,
}

// ScheduleConfig is the top-level `schedule` block: when a job runs.
type ScheduleConfig struct {
	// Cron is the cron expression. Standard five-field syntax and the
	// @hourly/@daily style descriptors are accepted.
	Cron string `yaml:"cron"`
	// Timezone is the IANA name the expression is evaluated in. Empty
	// means UTC.
	Timezone string `yaml:"timezone,omitempty"`
	// Missed selects what happens to occurrences that passed while the
	// monitor was down. Only skip is supported in v1.
	Missed string `yaml:"missed,omitempty"`
}

// JobConfig is the top-level `job` block: what a job runs and how.
type JobConfig struct {
	// Command is the argv run without a shell, relative to the project
	// directory.
	Command []string `yaml:"command"`
	// Timeout bounds one attempt as a duration. Empty means 30m.
	Timeout string `yaml:"timeout,omitempty"`
	// Retries is how many extra attempts run after a failed attempt.
	Retries int `yaml:"retries,omitempty"`
	// Concurrency controls overlapping runs of the same job.
	Concurrency *JobConcurrencyConfig `yaml:"concurrency,omitempty"`
}

// JobConcurrencyConfig holds the overlap policy for one job.
type JobConcurrencyConfig struct {
	// Policy is forbid (default) or allow.
	Policy string `yaml:"policy,omitempty"`
}

// ScheduleSettings is ScheduleConfig resolved.
type ScheduleSettings struct {
	Cron     string
	Timezone string
	Location *time.Location
	Missed   string
}

// JobSettings is JobConfig resolved.
type JobSettings struct {
	Command           []string
	Timeout           time.Duration
	Retries           int
	ConcurrencyPolicy string
}

// Resolve applies defaults. Call only on a config that passed validate.
func (s *ScheduleConfig) Resolve() ScheduleSettings {
	st := ScheduleSettings{
		Timezone: DefaultJobScheduleTimezone,
		Location: time.UTC,
		Missed:   MissedSkip,
	}
	if s == nil {
		return st
	}
	st.Cron = strings.TrimSpace(s.Cron)
	if tz := strings.TrimSpace(s.Timezone); tz != "" {
		st.Timezone = tz
		if loc, err := time.LoadLocation(tz); err == nil {
			st.Location = loc
		}
	}
	if missed := strings.TrimSpace(s.Missed); missed != "" {
		st.Missed = missed
	}
	return st
}

// Resolve applies defaults. Call only on a config that passed validate.
func (j *JobConfig) Resolve() JobSettings {
	st := JobSettings{
		Timeout:           DefaultJobTimeout,
		ConcurrencyPolicy: DefaultJobConcurrency,
	}
	if j == nil {
		return st
	}
	st.Command = append([]string(nil), j.Command...)
	if d, err := time.ParseDuration(j.Timeout); err == nil && d > 0 {
		st.Timeout = d
	}
	st.Retries = j.Retries
	if j.Concurrency != nil {
		if policy := strings.TrimSpace(j.Concurrency.Policy); policy != "" {
			st.ConcurrencyPolicy = policy
		}
	}
	return st
}

// IsJob reports whether this project declares type: job.
func (c *Config) IsJob() bool {
	return c.WorkloadType() == TypeJob
}

// validateJob checks the job-only constraints: a job is a one-shot command,
// so it has no port, no health probes, no process supervision and no deploy
// strategy. The schedule and job blocks are required.
func (c *Config) validateJob() error {
	if c.Port != 0 {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: port is not valid for type: job\nHint: jobs run a command and exit; remove the top-level port key")
	}
	if c.Health != nil {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: health is not valid for type: job\nHint: a job is one-shot; success is its exit status")
	}
	if c.Process != nil {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: process is only valid for type: worker\nHint: jobs are not supervised; retries and timeout live under job")
	}
	if c.Deploy != nil {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: deploy is not valid for type: job\nHint: a job is built as a version and run on schedule; there is no service to deploy")
	}
	if c.Schedule == nil {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: type: job requires a schedule block\nHint: add schedule.cron, for example cron: \"0 * * * *\"")
	}
	if c.Job == nil {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: type: job requires a job block\nHint: add job.command with the argv to run")
	}
	if err := c.Schedule.validate(); err != nil {
		return err
	}
	return c.Job.validate()
}

// validate checks the schedule block: a parseable cron expression, a real
// IANA timezone, and the supported missed policy.
func (s *ScheduleConfig) validate() error {
	spec := strings.TrimSpace(s.Cron)
	if spec == "" {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: schedule.cron is required\nHint: use standard five-field cron, for example \"0 3 * * *\"")
	}
	if strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=") {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: schedule.cron must not embed a timezone\nHint: set schedule.timezone instead")
	}
	if _, err := cron.ParseStandard(spec); err != nil {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: schedule.cron %q is not a valid cron expression\nHint: use five fields (minute hour day month weekday) or @hourly/@daily", s.Cron)
	}
	tz := strings.TrimSpace(s.Timezone)
	if s.Timezone != "" && tz == "" {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: schedule.timezone must not be blank\nHint: leave it out for UTC or name an IANA timezone such as Europe/Paris")
	}
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: schedule.timezone %q is not a known IANA timezone\nHint: use a name like UTC, Europe/Paris or America/New_York", s.Timezone)
		}
	}
	if missed := strings.TrimSpace(s.Missed); missed != "" && missed != MissedSkip {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: schedule.missed %q is not supported\nHint: only skip is supported; missed occurrences are skipped when the monitor was down", s.Missed)
	}
	return nil
}

// validate checks the job block: a non-empty argv, a positive timeout, a
// non-negative retry count and a known concurrency policy.
func (j *JobConfig) validate() error {
	if len(j.Command) == 0 || strings.TrimSpace(j.Command[0]) == "" {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: job.command is required\nHint: list the argv to run, for example command: [\"./backup\"]")
	}
	if j.Timeout != "" {
		d, err := time.ParseDuration(j.Timeout)
		if err != nil || d <= 0 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: job.timeout must be a positive duration like 30m, got %q", j.Timeout)
		}
	}
	if j.Retries < 0 {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: job.retries must be >= 0, got %d", j.Retries)
	}
	if j.Concurrency != nil {
		policy := strings.TrimSpace(j.Concurrency.Policy)
		if policy != "" && !supportedJobConcurrencyPolicies[policy] {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: invalid job.concurrency.policy %q\nHint: expected one of: forbid, allow", j.Concurrency.Policy)
		}
	}
	return nil
}

// validateRejectsJobKeys keeps service and worker files honest: job keys are
// rejected loudly instead of being parsed and ignored.
func (c *Config) validateRejectsJobKeys() error {
	if c.Schedule != nil {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: schedule is only valid for type: job\nHint: add type: job, or remove the schedule block")
	}
	if c.Job != nil {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: job is only valid for type: job\nHint: add type: job, or remove the job block")
	}
	return nil
}
