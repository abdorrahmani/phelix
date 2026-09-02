package errreport

import (
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/port"
)

func stubFindListener(t *testing.T, info port.ProcessInfo, ok bool) {
	t.Helper()
	old := FindListener
	FindListener = func(portNum int) (port.ProcessInfo, bool) {
		return info, ok
	}
	t.Cleanup(func() { FindListener = old })
}

func stubGoos(t *testing.T, goosValue string) {
	t.Helper()
	old := goos
	goos = goosValue
	t.Cleanup(func() { goos = old })
}

func portInUse(portNum int) error {
	return phelixerr.Newf(phelixerr.CodePortUnavailable,
		"port %d is already in use by another process", portNum)
}

func TestPortResolverIdentifiesProcess(t *testing.T) {
	stubGoos(t, "linux")
	stubFindListener(t, port.ProcessInfo{Name: "myapp", PID: 12345}, true)

	rep, ok := For(portInUse(8080))
	if !ok {
		t.Fatalf("port-in-use error not recognized")
	}
	if !strings.Contains(rep.Explanation, "Port 8080 is already being used") {
		t.Fatalf("Explanation missing port fact: %q", rep.Explanation)
	}
	if !strings.Contains(rep.Explanation, "myapp") || !strings.Contains(rep.Explanation, "12345") {
		t.Fatalf("Explanation missing process info: %q", rep.Explanation)
	}
	if !strings.Contains(rep.Command, ":8080") {
		t.Fatalf("Command should target port 8080: %q", rep.Command)
	}
	if !strings.Contains(rep.Command, "lsof") {
		t.Fatalf("linux inspection command should use lsof: %q", rep.Command)
	}
	if rep.DocsURL != "" {
		t.Fatalf("no Phelix docs URL exists for port errors; got %q", rep.DocsURL)
	}
	if rep.Code != phelixerr.CodePortUnavailable {
		t.Fatalf("Code = %s", rep.Code)
	}
}

func TestPortResolverNeverSuggestsKilling(t *testing.T) {
	stubGoos(t, "linux")
	stubFindListener(t, port.ProcessInfo{}, false)

	rep, ok := For(portInUse(3000))
	if !ok {
		t.Fatalf("port-in-use error not recognized")
	}
	all := rep.Title + rep.Explanation + rep.Suggestion + rep.Command
	for _, banned := range []string{"kill ", "kill -9", "fuser -k", "taskkill"} {
		if strings.Contains(all, banned) {
			t.Fatalf("report suggests a destructive command %q: %q", banned, all)
		}
	}
	if strings.Contains(rep.Explanation, "Currently listening") {
		t.Fatalf("unidentified listener must not render process info: %q", rep.Explanation)
	}
}

func TestPortResolverInspectionCommandsPerPlatform(t *testing.T) {
	cases := []struct {
		goosValue string
		contains  string
	}{
		{"linux", "sudo lsof -i :8080"},
		{"darwin", "lsof -i :8080"},
		{"windows", "netstat -ano | findstr :8080"},
	}
	for _, tc := range cases {
		t.Run(tc.goosValue, func(t *testing.T) {
			stubGoos(t, tc.goosValue)
			cmd := inspectPortCommand(8080)
			if cmd != tc.contains {
				t.Fatalf("goos=%s: command = %q, want %q", tc.goosValue, cmd, tc.contains)
			}
		})
	}
}

func TestPortResolverOtherPortUnavailableErrorsStayUnknown(t *testing.T) {
	stubGoos(t, "linux")
	stubFindListener(t, port.ProcessInfo{}, false)

	cases := []struct {
		name string
		err  error
	}{
		{"proxy bind failure", phelixerr.Wrapf(phelixerr.CodePortUnavailable,
			phelixerr.New(phelixerr.CodeConnection, "address already in use"),
			"proxy: listen on :%d", 9090)},
		{"port validation failure", phelixerr.Newf(phelixerr.CodePortUnavailable,
			"application failed port validation\n\nPhelix started the application with:\n\n    PORT=%d\n\nbut nothing is listening", 8080)},
		{"deploy allocation race", phelixerr.Newf(phelixerr.CodePortUnavailable,
			"deploy: allocate internal port %d", 45123)},
		{"different code, same words", phelixerr.New(phelixerr.CodeConnection,
			"port 8080 is already in use by another process")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := For(tc.err); ok {
				t.Fatalf("%s should stay unknown", tc.name)
			}
		})
	}
}
