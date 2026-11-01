package processex

import (
	"os"
	"os/exec"
	"testing"
)

func TestParseStatStarttime(t *testing.T) {
	line := "1234 (my app (v2) worker) S 1 1234 1234 0 -1 4194560 100 0 0 0 5 2 0 0 20 0 1 0 " +
		"567890123 34078720 2440 18446744073709551615 134217728 134217728 0 0 0 0 0 0 0 0 0 17 1 0 0 0 0 0"
	ticks, err := parseStatStarttime([]byte(line))
	if err != nil {
		t.Fatalf("parseStatStarttime: %v", err)
	}
	if ticks != 567890123 {
		t.Errorf("ticks = %d, want 567890123", ticks)
	}
}

func TestParseStatStarttimeMalformed(t *testing.T) {
	if _, err := parseStatStarttime([]byte("no parens here")); err == nil {
		t.Error("expected error for line without parens")
	}
	if _, err := parseStatStarttime([]byte("1 (x) S")); err == nil {
		t.Error("expected error for truncated line")
	}
}

func TestIdentityMatchesLiveProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	id, err := Identity(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.PID != cmd.Process.Pid || id.StartTime == 0 || id.ExePath == "" {
		t.Fatalf("incomplete identity: %+v", id)
	}
	if !Matches(id) {
		t.Error("live process should match its own identity")
	}

	forged := id
	forged.StartTime = id.StartTime + 1
	if Matches(forged) {
		t.Error("identity with wrong start time must not match")
	}
	forged = id
	forged.ExePath = "/bin/false"
	if Matches(forged) {
		t.Error("identity with wrong exe must not match")
	}
}

func TestMatchesDeadProcess(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn true: %v", err)
	}
	_, _ = cmd.Process.Wait()
	id := ProcessIdentity{PID: cmd.Process.Pid, StartTime: 42, ExePath: "/bin/true"}
	if Matches(id) {
		t.Error("dead process must not match")
	}
	if _, err := Identity(cmd.Process.Pid); err == nil {
		t.Error("Identity of a dead process must fail")
	}
}

func TestSameExe(t *testing.T) {
	if !sameExe("/bin/app_abc (deleted)", "/bin/app_abc") {
		t.Error("deleted marker must be tolerated")
	}
	if sameExe("", "/bin/app") || sameExe("/bin/app", "") {
		t.Error("empty paths must never match")
	}
	if sameExe("/bin/other", "/bin/app") {
		t.Error("different binaries must not match")
	}
	if _, err := os.Stat("/proc/self/exe"); err != nil {
		t.Skip("not a /proc platform build")
	}
}
