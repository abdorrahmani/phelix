package project

import (
	"strings"
	"testing"
	"time"
)

func loadWorker(t *testing.T, content string) (*Config, error) {
	t.Helper()
	return Load(write(t, content))
}

func TestLoadWorkerConfig(t *testing.T) {
	cfg, err := loadWorker(t, `
name: email-worker
type: worker

process:
  restart:
    policy: on-failure
    max_attempts: 10
    backoff:
      initial: 2s
      max: 60s
      multiplier: 2
  shutdown:
    signal: SIGTERM
    grace_period: 30s
  startup_timeout: 30s

health:
  mode: exec
  exec:
    command: ["./healthcheck"]
    interval: 10s
    timeout: 5s
    retries: 3

deploy:
  strategy: rolling
  replicas: 3
  rolling:
    max_surge: 1
    max_unavailable: 0
    drain_timeout: 60s

resources:
  cpu: 500m
  memory: 512Mi
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.IsWorker() || cfg.WorkloadType() != TypeWorker {
		t.Fatalf("type = %q", cfg.WorkloadType())
	}
	ps := cfg.Process.Resolve()
	if ps.RestartPolicy != RestartOnFailure || ps.MaxRestartAttempts != 10 {
		t.Errorf("restart = %+v", ps)
	}
	if ps.BackoffInitial != 2*time.Second || ps.BackoffMax != time.Minute || ps.BackoffMultiplier != 2 {
		t.Errorf("backoff = %+v", ps)
	}
	if ps.ShutdownSignal != ShutdownSIGTERM || ps.GracePeriod != 30*time.Second || ps.StartupTimeout != 30*time.Second {
		t.Errorf("shutdown/startup = %+v", ps)
	}
	hs := cfg.Health.ResolveWorker()
	if hs.Mode != WorkerHealthExec || len(hs.Command) != 1 || hs.Command[0] != "./healthcheck" {
		t.Errorf("health = %+v", hs)
	}
	if hs.Interval != 10*time.Second || hs.Timeout != 5*time.Second || hs.Retries != 3 {
		t.Errorf("health timings = %+v", hs)
	}
	rs := cfg.Deploy.Rolling.Resolve()
	if rs.MaxSurge != 1 || rs.MaxUnavailable != 0 || rs.DrainTimeout != time.Minute {
		t.Errorf("rolling = %+v", rs)
	}
}

func TestWorkerDefaults(t *testing.T) {
	cfg, err := loadWorker(t, `
name: w
type: worker
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ps := cfg.Process.Resolve()
	if ps.RestartPolicy != DefaultRestartPolicy || ps.MaxRestartAttempts != DefaultMaxRestartAttempts {
		t.Errorf("restart defaults = %+v", ps)
	}
	if ps.BackoffInitial != DefaultBackoffInitial || ps.BackoffMax != DefaultBackoffMax || ps.BackoffMultiplier != DefaultBackoffMultiplier {
		t.Errorf("backoff defaults = %+v", ps)
	}
	if ps.GracePeriod != DefaultShutdownGracePeriod || ps.StartupTimeout != DefaultStartupTimeout {
		t.Errorf("shutdown defaults = %+v", ps)
	}
	hs := cfg.Health.ResolveWorker()
	if hs.Mode != DefaultWorkerHealthMode || hs.Interval != DefaultWorkerExecInterval {
		t.Errorf("health defaults = %+v", hs)
	}
	if cfg.Deploy != nil {
		t.Errorf("worker without deploy block should have no deploy config, got %+v", cfg.Deploy)
	}
}

func TestAbsentTypeIsService(t *testing.T) {
	cfg, err := loadWorker(t, "name: api\nport: 8080\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkloadType() != TypeService || cfg.IsWorker() {
		t.Errorf("absent type should be service, got %q", cfg.WorkloadType())
	}
}

func TestWorkerRejections(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"port", "type: worker\nname: w\nport: 8080\n", "port is not valid"},
		{"blue-green", "type: worker\nname: w\ndeploy:\n  strategy: blue-green\n", "not valid for type: worker"},
		{"canary", "type: worker\nname: w\ndeploy:\n  strategy: canary\n", "not valid for type: worker"},
		{"docker", "type: worker\nname: w\ndeploy:\n  runtime: docker\n  strategy: rolling\n", "not supported for type: worker"},
		{"rollout", "type: worker\nname: w\ndeploy:\n  rollout:\n    canary: 10\n", "deploy.rollout is not valid"},
		{"autoscaling", "type: worker\nname: w\ndeploy:\n  autoscaling:\n    enabled: true\n", "deploy.autoscaling is not valid"},
		{"endpoints", "type: worker\nname: w\nhealth:\n  endpoints:\n    - name: d\n      path: /h\n", "health.endpoints is not valid"},
		{"exec without mode", "type: worker\nname: w\nhealth:\n  exec:\n    command: [\"./hc\"]\n", "health.exec requires health.mode: exec"},
		{"exec without command", "type: worker\nname: w\nhealth:\n  mode: exec\n", "requires a health.exec block"},
		{"bad mode", "type: worker\nname: w\nhealth:\n  mode: tcp-only\n", "invalid health.mode"},
		{"bad policy", "type: worker\nname: w\nprocess:\n  restart:\n    policy: sometimes\n", "invalid process.restart.policy"},
		{"bad multiplier", "type: worker\nname: w\nprocess:\n  restart:\n    backoff:\n      multiplier: 0.5\n", "multiplier must be >= 1"},
		{"bad signal", "type: worker\nname: w\nprocess:\n  shutdown:\n    signal: SIGKILL\n", "invalid process.shutdown.signal"},
		{"surge", "type: worker\nname: w\ndeploy:\n  rolling:\n    max_surge: 2\n", "max_surge must be 1"},
		{"unavailable", "type: worker\nname: w\ndeploy:\n  rolling:\n    max_unavailable: 1\n", "max_unavailable must be 0"},
		{"bad drain", "type: worker\nname: w\ndeploy:\n  rolling:\n    drain_timeout: later\n", "invalid duration"},
		{"unknown type", "type: daemon\nname: w\n", "invalid type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWorker(t, tc.content)
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestServiceRejectsWorkerKeys(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"process", "name: api\nport: 8080\nprocess:\n  restart:\n    policy: always\n", "process is only valid"},
		{"health mode", "name: api\nport: 8080\nhealth:\n  mode: process\n", "health.mode is only valid"},
		{"health exec", "name: api\nport: 8080\nhealth:\n  exec:\n    command: [\"./hc\"]\n", "health.exec is only valid"},
		{"deploy rolling", "name: api\nport: 8080\ndeploy:\n  rolling:\n    max_surge: 1\n", "deploy.rolling is only valid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWorker(t, tc.content)
			if err == nil {
				t.Fatalf("expected rejection, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestWorkerReplicasAllowedUnderClassic(t *testing.T) {
	cfg, err := loadWorker(t, "type: worker\nname: w\ndeploy:\n  strategy: classic\n  replicas: 2\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Deploy.Replicas != 2 {
		t.Errorf("replicas = %d", cfg.Deploy.Replicas)
	}
}
