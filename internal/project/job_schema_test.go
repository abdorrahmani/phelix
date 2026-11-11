package project

import (
	"strings"
	"testing"
	"time"
)

func loadJob(t *testing.T, content string) (*Config, error) {
	t.Helper()
	return Load(write(t, content))
}

func TestLoadJobConfig(t *testing.T) {
	cfg, err := loadJob(t, `
name: nightly
type: job
schedule:
  cron: "0 3 * * *"
  timezone: Europe/Paris
  missed: skip
job:
  command: ["./backup", "--full"]
  timeout: 15m
  retries: 2
  concurrency:
    policy: allow
resources:
  cpu: 500m
  memory: 256Mi
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.IsJob() || cfg.WorkloadType() != TypeJob {
		t.Fatalf("type = %q", cfg.WorkloadType())
	}
	ss := cfg.Schedule.Resolve()
	if ss.Cron != "0 3 * * *" || ss.Timezone != "Europe/Paris" || ss.Missed != MissedSkip {
		t.Errorf("schedule = %+v", ss)
	}
	if ss.Location == nil || ss.Location.String() != "Europe/Paris" {
		t.Errorf("location = %v", ss.Location)
	}
	js := cfg.Job.Resolve()
	if len(js.Command) != 2 || js.Command[0] != "./backup" || js.Command[1] != "--full" {
		t.Errorf("command = %+v", js.Command)
	}
	if js.Timeout != 15*time.Minute || js.Retries != 2 || js.ConcurrencyPolicy != ConcurrencyAllow {
		t.Errorf("job = %+v", js)
	}
}

func TestJobDefaults(t *testing.T) {
	cfg, err := loadJob(t, `
name: nightly
type: job
schedule:
  cron: "@daily"
job:
  command: ["./backup"]
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ss := cfg.Schedule.Resolve()
	if ss.Timezone != DefaultJobScheduleTimezone || ss.Location != time.UTC {
		t.Errorf("schedule defaults = %+v", ss)
	}
	if ss.Missed != MissedSkip {
		t.Errorf("missed default = %q", ss.Missed)
	}
	js := cfg.Job.Resolve()
	if js.Timeout != DefaultJobTimeout || js.Retries != 0 || js.ConcurrencyPolicy != DefaultJobConcurrency {
		t.Errorf("job defaults = %+v", js)
	}
}

func TestJobRejections(t *testing.T) {
	const valid = `
name: nightly
type: job
schedule:
  cron: "0 3 * * *"
job:
  command: ["./backup"]
`
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"missing schedule", "name: n\ntype: job\njob:\n  command: [\"./b\"]\n", "requires a schedule block"},
		{"missing job", "name: n\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\n", "requires a job block"},
		{"missing cron", "name: n\ntype: job\nschedule:\n  timezone: UTC\njob:\n  command: [\"./b\"]\n", "schedule.cron is required"},
		{"bad cron", "name: n\ntype: job\nschedule:\n  cron: \"every day\"\njob:\n  command: [\"./b\"]\n", "not a valid cron expression"},
		{"cron fields", "name: n\ntype: job\nschedule:\n  cron: \"0 3 * *\"\njob:\n  command: [\"./b\"]\n", "not a valid cron expression"},
		{"embedded timezone", "name: n\ntype: job\nschedule:\n  cron: \"CRON_TZ=UTC 0 3 * * *\"\njob:\n  command: [\"./b\"]\n", "must not embed a timezone"},
		{"bad timezone", "name: n\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\n  timezone: Mars/Olympus\njob:\n  command: [\"./b\"]\n", "not a known IANA timezone"},
		{"blank timezone", "name: n\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\n  timezone: \"  \"\njob:\n  command: [\"./b\"]\n", "schedule.timezone must not be blank"},
		{"missed run", "name: n\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\n  missed: run\njob:\n  command: [\"./b\"]\n", "only skip is supported"},
		{"empty command", "name: n\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\njob:\n  command: []\n", "job.command is required"},
		{"blank command", "name: n\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\njob:\n  command: [\"  \"]\n", "job.command is required"},
		{"zero timeout", "name: n\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\njob:\n  command: [\"./b\"]\n  timeout: 0s\n", "positive duration"},
		{"negative timeout", "name: n\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\njob:\n  command: [\"./b\"]\n  timeout: -5m\n", "positive duration"},
		{"bad timeout", "name: n\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\njob:\n  command: [\"./b\"]\n  timeout: soon\n", "positive duration"},
		{"negative retries", "name: n\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\njob:\n  command: [\"./b\"]\n  retries: -1\n", "job.retries must be >= 0"},
		{"bad policy", "name: n\ntype: job\nschedule:\n  cron: \"0 3 * * *\"\njob:\n  command: [\"./b\"]\n  concurrency:\n    policy: queue\n", "invalid job.concurrency.policy"},
		{"port", valid + "port: 8080\n", "port is not valid for type: job"},
		{"health", valid + "health:\n  endpoints:\n    - name: h\n      path: /h\n", "health is not valid for type: job"},
		{"process", valid + "process:\n  restart:\n    policy: always\n", "process is only valid for type: worker"},
		{"deploy", valid + "deploy:\n  strategy: rolling\n", "deploy is not valid for type: job"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadJob(t, tc.content)
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestNonJobRejectsJobKeys(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"service schedule", "name: api\nport: 8080\nschedule:\n  cron: \"0 3 * * *\"\n", "schedule is only valid for type: job"},
		{"service job", "name: api\nport: 8080\njob:\n  command: [\"./b\"]\n", "job is only valid for type: job"},
		{"worker schedule", "name: w\ntype: worker\nschedule:\n  cron: \"0 3 * * *\"\n", "schedule is only valid for type: job"},
		{"worker job", "name: w\ntype: worker\njob:\n  command: [\"./b\"]\n", "job is only valid for type: job"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadJob(t, tc.content)
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}
