package deploy

import (
	"os"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/health"
)

// TestCandidateHealthFailure_DockerLogsTail: a failed docker candidate must
// surface its `docker logs` output in the health-failure error — the docker
// runtime's equivalent of the native on-disk log tail — routed through the
// injectable dockerRunner seam (no real daemon), and redacted.
func TestCandidateHealthFailure_DockerLogsTail(t *testing.T) {
	fd := &fakeDocker{logs: "thread 'main' panicked: Address already in use (os error 98); token=abcdef1234567890\n"}
	proc := &dockerProcess{id: "c0ffee", run: fd.runner()}

	err := candidateHealthFailure(proc, "billing:v3", 32769,
		health.Tier1HTTPPath, pathHealth("/ping"), health.ErrCandidateExited)

	joined := err.Error() + chainText(err)
	if !strings.Contains(joined, "instance output (tail):") {
		t.Fatalf("docker failure must include the log-tail section: %v", err)
	}
	if !strings.Contains(joined, "panicked") || !strings.Contains(joined, "Address already in use") {
		t.Fatalf("docker failure must contain the container's actual output: %v", err)
	}
	// The tail passes through phelixerr.Redact, same as the native path.
	if strings.Contains(joined, "abcdef1234567890") {
		t.Fatalf("docker log tail must be redacted: %v", err)
	}
	// The tail came from `docker logs <id>` via the seam.
	sawLogs := false
	for _, c := range fd.calls {
		if len(c) >= 2 && c[0] == "logs" && c[len(c)-1] == "c0ffee" {
			sawLogs = true
		}
	}
	if !sawLogs {
		t.Fatalf("expected a `docker logs c0ffee` call, got %v", fd.calls)
	}
}

// TestCandidateHealthFailure_NativeUnchanged: a native candidate (no ContainerID
// /Logs) still reports the on-disk instance-log tail and never triggers a docker
// call — the native path is unchanged by the docker enrichment.
func TestCandidateHealthFailure_NativeUnchanged(t *testing.T) {
	resetHome(t)
	home, _ := os.UserHomeDir()
	dir := home + "/.phelix/logs"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/deploy_fakeapp_4243.log",
		[]byte("panic: nil map write; token=abcdef1234567890\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// selfProc is a plain native process handle (no ContainerID/Logs method).
	proc := &selfProc{pid: os.Getpid()}
	err := candidateHealthFailure(proc, "/tmp/fakeapp", 4243,
		health.Tier1HTTPPath, pathHealth("/ping"), health.ErrCandidateExited)

	joined := err.Error() + chainText(err)
	if !strings.Contains(joined, "probe target: http://127.0.0.1:4243") {
		t.Fatalf("native probe target unchanged prefix missing: %v", err)
	}
	if !strings.Contains(joined, "panic: nil map write") {
		t.Fatalf("native failure must include the on-disk log tail: %v", err)
	}
	if strings.Contains(joined, "abcdef1234567890") {
		t.Fatalf("native log tail must be redacted: %v", err)
	}
}

// TestProbeTarget_IncludesPathAndTier: the reported probe target must match what
// was actually checked — the configured path for Tier 1, "/" for Tier 2, and a
// bare host:port for the TCP/PID tiers — so an operator on /ping is not misled
// into thinking Phelix probed "/".
func TestProbeTarget_IncludesPathAndTier(t *testing.T) {
	cases := []struct {
		name string
		tier health.Tier
		cfg  *health.DeployTierConfig
		want string
	}{
		{"tier1 configured path", health.Tier1HTTPPath, pathHealth("/ping"), "http://127.0.0.1:41000/ping"},
		{"tier1 nil cfg defaults slash", health.Tier1HTTPPath, nil, "http://127.0.0.1:41000/"},
		{"tier2 http any", health.Tier2HTTPAny, nil, "http://127.0.0.1:41000/"},
		{"tier3 tcp bare host", health.Tier3TCP, nil, "127.0.0.1:41000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := probeTarget(tc.tier, tc.cfg, 41000)
			if !strings.HasPrefix(got, tc.want) {
				t.Fatalf("probeTarget = %q, want prefix %q", got, tc.want)
			}
			// The selected tier is always named alongside the target.
			if !strings.Contains(got, tc.tier.String()) {
				t.Fatalf("probeTarget %q must name the tier %q", got, tc.tier.String())
			}
		})
	}
	// Tier 3 must not fabricate an HTTP scheme it never dialed.
	if got := probeTarget(health.Tier3TCP, nil, 41000); strings.Contains(got, "http://") {
		t.Fatalf("tier 3 is a TCP check, must not report an http URL: %q", got)
	}
}
