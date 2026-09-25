package deploy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/health"
)

// TestDockerProcessAlive: liveness is asked of Docker, not the host process
// table. A running container is alive; a genuinely exited or removed one is
// dead (fast-fail preserved); a transient inspect error is treated as alive so
// a daemon blip does not abort a healthy deploy.
func TestDockerProcessAlive(t *testing.T) {
	cases := []struct {
		name    string
		inspect string
		err     error
		want    bool
	}{
		{"running", "true\n", nil, true},
		{"exited", "false\n", nil, false},
		{"removed container", "", errors.New("Error: No such container: c1"), false},
		{"transient daemon error", "", errors.New("timeout talking to daemon"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fd := &fakeDocker{inspect: tc.inspect}
			if tc.err != nil {
				fd.errs = map[string]error{"inspect": tc.err}
			}
			p := &dockerProcess{id: "c1", run: fd.runner()}
			if got := p.Alive(); got != tc.want {
				t.Fatalf("Alive() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDockerCandidate_RunningIsHealthyNotExited: the end-to-end regression. A
// running container (State.Running=true) routed through deployResolver must NOT
// be declared exited — Tier3None's probe IS the liveness check, so a running
// container passes. Before the fix the host-PID signal-0 check returned EPERM
// (container owned by uid 65532) and every live candidate failed here.
func TestDockerCandidate_RunningIsHealthyNotExited(t *testing.T) {
	fd := &fakeDocker{inspect: "true\n"}
	proc := &dockerProcess{id: "cafe", run: fd.runner()}
	cfg := &health.DeployTierConfig{Mode: health.TierModeNone, Interval: "2ms", Retries: 1, Timeout: "1s"}
	if err := health.WaitForHealthy(context.Background(), health.Tier3None, cfg,
		"127.0.0.1:8080", 4242, deployResolver(proc)); err != nil {
		t.Fatalf("running container must pass, got %v", err)
	}
}

// TestDockerCandidate_ExitedFailsFast: a container that genuinely exited
// (State.Running=false) must still fast-fail as ErrCandidateExited rather than
// burning the whole timeout.
func TestDockerCandidate_ExitedFailsFast(t *testing.T) {
	fd := &fakeDocker{inspect: "false\n"}
	proc := &dockerProcess{id: "dead", run: fd.runner()}
	cfg := &health.DeployTierConfig{Mode: health.TierModeTCPOnly, Interval: "2ms", Retries: 1, Timeout: "5s"}
	start := time.Now()
	err := health.WaitForHealthy(context.Background(), health.Tier3TCP, cfg,
		"127.0.0.1:8080", 4242, deployResolver(proc))
	if !errors.Is(err, health.ErrCandidateExited) {
		t.Fatalf("exited container must fast-fail as ErrCandidateExited, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("must fail fast, took %s", time.Since(start))
	}
}

// TestDeployResolver_NativeIsDefault: a native process (no Alive method) yields
// a nil resolver, so WaitForHealthy keeps the default (EPERM-safe) host-PID
// checker — native behavior is unchanged.
func TestDeployResolver_NativeIsDefault(t *testing.T) {
	if r := deployResolver(&selfProc{pid: 1}); r != nil {
		t.Fatalf("native process must map to the default resolver (nil), got %v", r)
	}
}
