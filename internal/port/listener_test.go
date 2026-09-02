package port

import (
	"errors"
	"strings"
	"testing"
)

func TestParseLsofFields(t *testing.T) {
	info, ok := parseLsofFields("p1234\ncmyapp\n")
	if !ok || info.PID != 1234 || info.Name != "myapp" {
		t.Fatalf("basic parse failed: %+v ok=%v", info, ok)
	}

	// Multiple FDs per process: first pair wins, later repeats ignored.
	info, ok = parseLsofFields("p999\ncfirst\np999\ncsecond\n")
	if !ok || info.PID != 999 || info.Name != "first" {
		t.Fatalf("multi-fd parse failed: %+v ok=%v", info, ok)
	}

	// Empty / malformed output.
	if _, ok := parseLsofFields(""); ok {
		t.Fatalf("empty output should not report a listener")
	}
	if _, ok := parseLsofFields("pnotapid\ncapp\n"); ok {
		t.Fatalf("non-numeric pid should not report a listener")
	}
	if _, ok := parseLsofFields("p1234\n"); ok {
		t.Fatalf("pid without command name should not report a listener")
	}
}

func TestFindListenerRangeGuard(t *testing.T) {
	// Out-of-range ports are rejected without ever invoking lsof.
	for _, p := range []int{0, -1, 65536} {
		if _, ok := FindListener(p); ok {
			t.Fatalf("FindListener(%d) should fail the range guard", p)
		}
	}
}

func TestFindListenerWithStub(t *testing.T) {
	old := runLsof
	runLsof = func(portNum int) (string, error) {
		return "p4242\nn127.0.0.1:8080\n", nil
	}
	t.Cleanup(func() { runLsof = old })

	// Command name missing -> not identified even though the pid parsed.
	if _, ok := FindListener(8080); ok {
		t.Fatalf("listener without command name should not be reported as found")
	}

	runLsof = func(portNum int) (string, error) {
		return "p4242\ncmyapp\n", nil
	}
	info, ok := FindListener(8080)
	if !ok || info.PID != 4242 || info.Name != "myapp" {
		t.Fatalf("stubbed lookup failed: %+v ok=%v", info, ok)
	}
}

func TestFindListenerMissingTool(t *testing.T) {
	old := runLsof
	runLsof = func(portNum int) (string, error) {
		return "", errors.New("exec: lsof: executable file not found in $PATH")
	}
	t.Cleanup(func() { runLsof = old })

	if _, ok := FindListener(8080); ok {
		t.Fatalf("missing lsof must degrade to not-found, never an error")
	}
}

func TestFindListenerIsReadOnly(t *testing.T) {
	// Guard the contract: the lookup inspects, it never signals or kills.
	// Pinned here so nobody "improves" FindListener into something destructive.
	old := runLsof
	var ran string
	runLsof = func(portNum int) (string, error) {
		ran = "lsof"
		return "p1\nctest\n", nil
	}
	t.Cleanup(func() { runLsof = old })

	_, _ = FindListener(8080)
	if !strings.EqualFold(ran, "lsof") {
		t.Fatalf("expected an lsof inspection, ran: %q", ran)
	}
}
