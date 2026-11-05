package deploy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/secrets"
)

func TestCoverageRetainsRuntimeUntilInstanceStops(t *testing.T) {
	txnTestEnv(t)
	seedSecret(t, "db", "stored-coverage-value")
	if err := secrets.ReplaceBindings(txnTestAppID, []secrets.Binding{{Secret: "db", Env: "DB"}}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	cmd.Env = append(os.Environ(), "DB=old-coverage-value")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	info := app.AppListItem{ID: txnTestAppID, Name: "coverage", PID: cmd.Process.Pid, Status: "running", SecretEnvs: []string{"DB"}}
	if err := RefreshSecretCoverage(info); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"stored-coverage-value", "old-coverage-value"} {
		if phelixerr.Redact(value) != "***" {
			t.Fatal("stored or running value lacks coverage")
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	info.PID, info.Status = 0, "stopped"
	if err := RefreshSecretCoverage(info); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"stored-coverage-value", "old-coverage-value"} {
		if phelixerr.Redact(value) != value {
			t.Fatal("stopped app retained credential references")
		}
	}
}

func TestCoverageDoesNotDegradePlainAppForBrokenJournal(t *testing.T) {
	txnTestEnv(t)
	path, err := statePath("plain-coverage")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RefreshSecretCoverage(app.AppListItem{ID: txnTestAppID, Name: "plain-coverage"}); err != nil {
		t.Fatal(err)
	}
	if state, _ := secrets.SecurityOf(txnTestAppID); state != secrets.SecurityHealthy {
		t.Fatal("plain app was degraded by unrelated journal")
	}
}

func TestRuntimeWithoutSecretsNeedsNoProcessInspection(t *testing.T) {
	set, err := RuntimeSecrets(&Instance{PID: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	if len(set.Values()) != 0 {
		t.Fatal("empty runtime unexpectedly held credentials")
	}
	if _, err := secrets.PinBindings(context.Background(), txnTestAppID, nil); err != nil {
		t.Fatal(err)
	}
}
