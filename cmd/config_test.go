package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/project"
)

// withProjectFile runs fn from a directory holding content as phelix.yaml.
func withProjectFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, project.FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	prevWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWd) })
	return dir
}

func TestConfigLintValid_Succeeds(t *testing.T) {
	dir := withProjectFile(t, "name: demo\nport: 3000\nhealth:\n  endpoints:\n    - name: default\n      path: /health\n")

	out := captureStdout(t, func() {
		if err := configLintCmd.RunE(configLintCmd, nil); err != nil {
			t.Errorf("lint on a valid config: %v", err)
		}
	})
	if !strings.Contains(out, "is valid") || !strings.Contains(out, filepath.Join(dir, project.FileName)) {
		t.Fatalf("valid lint output = %q", out)
	}
}

func TestConfigLintJSON_EmitsResultEnvelope(t *testing.T) {
	dir := withProjectFile(t, "name: demo\nport: 3000\n")
	prevJSON := configLintJSON
	configLintJSON = true
	defer func() { configLintJSON = prevJSON }()
	defer machine.LeaveJSON()

	out := captureStdout(t, func() {
		if err := configLintCmd.RunE(configLintCmd, nil); err != nil {
			t.Errorf("lint --json: %v", err)
		}
	})
	env := decodeEnvelope(t, out)
	raw, _ := json.Marshal(env.Result)
	var result configLintResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("result is not a configLintResult: %v", err)
	}
	if !result.Valid || result.Path != filepath.Join(dir, project.FileName) {
		t.Fatalf("result = %+v", result)
	}
	if env.OperationID != "" {
		t.Fatal("lint is read-only: it must never fabricate an operation id")
	}
}

func TestConfigLintInvalid_SurfacesConfigurationError(t *testing.T) {
	withProjectFile(t, "name: demo\ndeploy:\n  strategy: nonsense\n")

	err := configLintCmd.RunE(configLintCmd, nil)
	if !phelixerr.IsCode(err, phelixerr.CodeConfiguration) {
		t.Fatalf("invalid config code = %v, want CONFIGURATION_ERROR", phelixerr.CodeOf(err))
	}
	if got := ExitCodeFor(err); got != ExitConfig {
		t.Fatalf("invalid config exit = %d, want %d", got, ExitConfig)
	}
}

func TestConfigLintMissing_ReportsNotFound(t *testing.T) {
	prevWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(prevWd) }()

	if err := configLintCmd.RunE(configLintCmd, nil); !phelixerr.IsCode(err, phelixerr.CodeNotFound) {
		t.Fatalf("missing config code = %v, want NOT_FOUND", phelixerr.CodeOf(err))
	}
}

func TestConfigSchema_PrintsTheSameDocumentAsTheGenerator(t *testing.T) {
	want, err := project.SchemaJSON()
	if err != nil {
		t.Fatalf("SchemaJSON: %v", err)
	}
	out := captureStdout(t, func() {
		if err := configSchemaCmd.RunE(configSchemaCmd, nil); err != nil {
			t.Errorf("config schema: %v", err)
		}
	})
	if out != string(want) {
		t.Fatal("`phelix config schema` must print the generator's document unchanged")
	}
}
