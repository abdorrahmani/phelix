package app

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// writeFixtureApp writes a tiny Go server, builds it as app_<id> inside dir,
// and registers it in the manager state so startApplicationProcess can run it.
func writeFixtureApp(t *testing.T, m *AppManager, id string, portAware bool) string {
	t.Helper()
	dir := t.TempDir()

	src := `package main

import (
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	http.ListenAndServe(":"+port, nil)
}
`
	if !portAware {
		// Bind an ephemeral port now so the fixture hardcodes a real, known
		// port; Phelix will be asked for a different one.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skipf("cannot allocate ephemeral port: %v", err)
		}
		hardPort := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		src = fmt.Sprintf(`package main

import "net/http"

func main() {
	http.ListenAndServe(":%d", nil)
}
`, hardPort)
		t.Cleanup(func() {
			// Ensure no fixture instance outlives the test even on failure.
			exec.Command("pkill", "-f", fmt.Sprintf("app_%s", id)).Run()
		})
	}

	mainGo := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainGo, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, fmt.Sprintf("app_%s", id))
	out, err := exec.Command("go", "build", "-o", bin, mainGo).CombinedOutput()
	if err != nil {
		t.Skipf("cannot build fixture (no go toolchain?): %v\n%s", err, out)
	}

	logFile := filepath.Join(dir, "app.log")
	m.Apps[id] = &AppInfo{
		ID:        id,
		Name:      id,
		Directory: dir,
		LogFile:   logFile,
	}
	return bin
}

func TestStartProcessPortInjection(t *testing.T) {
	m := &AppManager{Apps: map[string]*AppInfo{}}
	id := "porttest"
	writeFixtureApp(t, m, id, true)

	if err := m.startApplicationProcess(id, id, 45671, filepath.Join(m.Apps[id].Directory, "app.log")); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.StopApplication(id)

	// The app must actually listen on the requested port.
	conn, err := net.DialTimeout("tcp", "127.0.0.1:45671", timeoutForTest())
	if err != nil {
		t.Fatalf("nothing listening on :45671 after PORT=45671 injection: %v", err)
	}
	conn.Close()
}

func TestStartProcessDetectsHardcodedPort(t *testing.T) {
	m := &AppManager{Apps: map[string]*AppInfo{}}
	id := "hardcoded"
	writeFixtureApp(t, m, id, false)

	err := m.startApplicationProcess(id, id, 45672, filepath.Join(m.Apps[id].Directory, "app.log"))
	if err == nil {
		m.StopApplication(id)
		t.Fatal("start succeeded for hardcoded-port app, want failure")
	}
	if code := phelixerr.CodeOf(err); code != phelixerr.CodePortUnavailable {
		t.Errorf("code = %q, want PORT_UNAVAILABLE", code)
	}
	msg := err.Error()
	for _, want := range []string{"phelix doctor", "PORT=", ":45672"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}

	// The failed process must not be left running.
	if m.isProcessRunning(m.Apps[id].PID) {
		m.StopApplication(id)
		t.Error("failed instance still running after port validation failure")
	}
}

func TestPortEnvOverrideWins(t *testing.T) {
	// Simulate a pre-existing conflicting PORT in our own environment; the
	// child must receive Phelix's value, verified by the listener check.
	os.Setenv("PORT", "1")
	defer os.Unsetenv("PORT")

	m := &AppManager{Apps: map[string]*AppInfo{}}
	id := "envoverride"
	writeFixtureApp(t, m, id, true)

	if err := m.startApplicationProcess(id, id, 45673, filepath.Join(m.Apps[id].Directory, "app.log")); err != nil {
		t.Fatalf("start with inherited PORT=1: %v", err)
	}
	defer m.StopApplication(id)

	conn, err := net.DialTimeout("tcp", "127.0.0.1:45673", timeoutForTest())
	if err != nil {
		t.Fatalf("Phelix PORT did not override inherited env: %v", err)
	}
	conn.Close()
}

func TestExitsImmediatelyReported(t *testing.T) {
	m := &AppManager{Apps: map[string]*AppInfo{}}
	id := "exitfast"
	dir := t.TempDir()

	src := "package main\n\nfunc main() {}\n"
	mainGo := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainGo, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, fmt.Sprintf("app_%s", id))
	if out, err := exec.Command("go", "build", "-o", bin, mainGo).CombinedOutput(); err != nil {
		t.Skipf("cannot build fixture: %v\n%s", err, out)
	}

	m.Apps[id] = &AppInfo{ID: id, Name: id, Directory: dir}

	err := m.startApplicationProcess(id, id, 45674, filepath.Join(dir, "app.log"))
	if err == nil {
		t.Fatal("immediately-exiting app reported as success")
	}
	if code := phelixerr.CodeOf(err); code != phelixerr.CodeProcessFailed {
		t.Errorf("code = %q, want PROCESS_FAILED", code)
	}
	if !strings.Contains(err.Error(), "exited immediately") {
		t.Errorf("message should say the process exited: %s", err)
	}
}

func timeoutForTest() time.Duration { return 2 * time.Second }
