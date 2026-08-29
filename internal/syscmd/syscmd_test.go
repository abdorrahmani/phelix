package syscmd

import (
	"strings"
	"testing"
)

// TestExecRunnerRejectsUnknownCommands pins the whitelist: only systemctl
// and sudo may ever be executed, regardless of what a caller passes.
func TestExecRunnerRejectsUnknownCommands(t *testing.T) {
	runner := ExecRunner{}
	for _, name := range []string{"sh", "bash", "curl", "rm", "mv", "install", "eval", "", "SYSTEMCTL"} {
		_, err := runner.Run(name, "arg")
		if err == nil {
			t.Fatalf("ExecRunner.Run(%q) unexpectedly succeeded", name)
		}
		if !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("ExecRunner.Run(%q) error should be a whitelist rejection: %v", name, err)
		}
	}
}
