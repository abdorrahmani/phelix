package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestLogTailRedactsBeforeTruncation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	value := "credential-whose-suffix-must-not-leak"
	release := phelixerr.AcquireSecretValues([]string{value})
	defer release()
	path := instanceLogPath(home, "/app", 9123)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("prefix "+value), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := instanceLogTail("/app", 9123, 12); strings.Contains(got, "leak") || !strings.Contains(got, "***") {
		t.Fatalf("tail exposed a secret suffix: %q", got)
	}
}

func TestProbeOutputWithholdsTruncatedSecrets(t *testing.T) {
	value := strings.Repeat("credential", 600)
	release := phelixerr.AcquireSecretValues([]string{value})
	defer release()
	out := &boundedBuffer{max: 4096}
	for i := 0; i < len(value); i += 100 {
		end := min(i+100, len(value))
		_, _ = out.Write([]byte(value[i:end]))
	}
	if strings.Contains(out.String(), "credential") {
		t.Fatal("truncated credential exposed")
	}
}

func TestDockerArgsHideInlineValues(t *testing.T) {
	got := redactDockerArgs([]string{"run", "--env", "DB=opaque-value", "--label", "owner=private-owner", "image"})
	if got != "run --env DB=*** --label owner=*** image" {
		t.Fatalf("unsafe argv: %s", got)
	}
}

func TestDockerTailAcquiresCredentialsAfterInstanceExit(t *testing.T) {
	value := "retired-container-credential"
	proc := &dockerProcess{id: "retired", secretEnvs: []string{"DB"}, run: func(_ context.Context, args ...string) (string, error) {
		if args[0] == "inspect" {
			return `["DB=retired-container-credential"]`, nil
		}
		return value, nil
	}}
	if got := proc.Logs(context.Background(), 4096); got != "***" {
		t.Fatal("retired container log exposed credentials")
	}
	if phelixerr.Redact(value) != value {
		t.Fatal("log tail retained temporary ownership")
	}
}

func TestRollbackVerificationMasksBeforePersistence(t *testing.T) {
	txnTestEnv(t)
	value := "rollback-verification-private"
	release := phelixerr.AcquireSecretValues([]string{value})
	defer release()
	verification := &RollbackVerification{Error: value}
	RecordRollbackResult("masked-verification", 2, 1, "classic", "", verification, nil)
	path, err := rollbackHistoryPath("masked-verification")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), value) || verification.Error != value {
		t.Fatal("history exposed value or changed caller data")
	}
}
