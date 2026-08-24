package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/project"
)

func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const goPORTMain = `package main

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

const goHardcodedMain = `package main

import "net/http"

func main() {
	http.ListenAndServe(":3000", nil)
}
`

func TestDoctorGoProjectWithConfig(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod":      "module example.com/api\n\ngo 1.21\n",
		"main.go":     goPORTMain,
		"phelix.yaml": "name: api\nport: 4000\n",
	})

	results := runDoctorChecks(dir)
	byName := map[string]checkResult{}
	for _, r := range results {
		byName[r.name] = r
	}

	if !byName["Project detected"].pass || !strings.Contains(byName["Project detected"].detail, "go") {
		t.Errorf("project detection: %+v", byName["Project detected"])
	}
	if !byName["phelix.yaml"].pass || !byName["Application name"].pass || !byName["Configured port"].pass {
		t.Errorf("config checks failed: %+v %+v %+v",
			byName["phelix.yaml"], byName["Application name"], byName["Configured port"])
	}
	if !byName["PORT configuration"].pass {
		t.Errorf("PORT configuration should pass for PORT-aware app, got %+v", byName["PORT configuration"])
	}
}

func TestDoctorDetectsHardcodedPort(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod":  "module example.com/api\n\ngo 1.21\n",
		"main.go": goHardcodedMain,
	})

	results := runDoctorChecks(dir)
	var portCheck checkResult
	for _, r := range results {
		if r.name == "PORT configuration" {
			portCheck = r
		}
	}
	if portCheck.pass || portCheck.warn {
		t.Errorf("hardcoded :3000 must fail, got %+v", portCheck)
	}
	if !strings.Contains(portCheck.detail, ":3000") {
		t.Errorf("detail should name the port, got %q", portCheck.detail)
	}
	if !strings.Contains(portCheck.message, "os.Getenv") {
		t.Errorf("message should show the PORT idiom, got %q", portCheck.message)
	}
}

func TestDoctorMissingProject(t *testing.T) {
	dir := t.TempDir()
	results := runDoctorChecks(dir)
	if len(results) != 1 || results[0].pass {
		t.Errorf("empty dir should yield single failing detection, got %+v", results)
	}
}

func TestDoctorInconclusiveIsWarning(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod":  "module example.com/api\n\ngo 1.21\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	results := runDoctorChecks(dir)
	for _, r := range results {
		if r.name == "PORT configuration" && !r.warn {
			t.Errorf("no-listener no-PORT should warn not fail: %+v", r)
		}
	}
}

func TestDoctorMalformedConfigFails(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod":      "module example.com/api\n\ngo 1.21\n",
		"main.go":     goPORTMain,
		"phelix.yaml": "name: [\n",
	})
	results := runDoctorChecks(dir)
	for _, r := range results {
		if r.name == "phelix.yaml" && (r.pass || r.warn) {
			t.Errorf("malformed config must fail: %+v", r)
		}
	}
}

func TestInitWritesValidConfig(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod":  "module example.com/api\n\ngo 1.21\n",
		"main.go": goPORTMain,
	})
	// chdir so init's os.Getwd sees the fixture.
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)

	initName, initPort, initYes = "api", 4000, true
	if err := InitCmd.RunE(InitCmd, nil); err != nil {
		t.Fatalf("init RunE: %v", err)
	}

	cfg, err := project.Load(dir)
	if err != nil {
		t.Fatalf("created config does not load: %v", err)
	}
	if cfg.Name != "api" || cfg.Port != 4000 {
		t.Errorf("cfg = %+v, want {api 4000}", cfg)
	}
}

func TestInitDoesNotTouchSource(t *testing.T) {
	mainSrc := goHardcodedMain
	dir := writeProject(t, map[string]string{
		"go.mod":  "module example.com/api\n\ngo 1.21\n",
		"main.go": mainSrc,
	})
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)

	initName, initPort, initYes = "api", 3000, true
	if err := InitCmd.RunE(InitCmd, nil); err != nil {
		t.Fatalf("init RunE: %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if string(got) != mainSrc {
		t.Error("init modified main.go — forbidden")
	}
}

func TestInitRejectsInvalidPort(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod": "module example.com/api\n\ngo 1.21\n",
	})
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)

	initName, initPort, initYes = "api", 70000, true
	if err := InitCmd.RunE(InitCmd, nil); err == nil {
		t.Error("port 70000 accepted by init")
	}
}
