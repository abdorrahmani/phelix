package deploy

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/secrets"
)

func TestDockerSecretLifecycle(t *testing.T) {
	if os.Getenv("PHELIX_TEST_DOCKER") != "1" {
		t.Skip("set PHELIX_TEST_DOCKER=1 to use local Docker")
	}
	txnTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	dockerfile := "FROM alpine:3\nCMD [\"sh\",\"-c\",\"printf '%s\\n' \\\"$DB\\\"; trap 'exit 0' TERM; while :; do sleep 1; done\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0600); err != nil {
		t.Fatal(err)
	}
	image := "phelix-secret-test-" + strings.TrimPrefix(secrets.NewTxnID(), "sec_")
	if out, err := exec.CommandContext(ctx, "docker", "build", "-q", "-t", image, dir).CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", image).Run() })
	value := "container-private-first\ncontainer-private-second\n"
	seedSecret(t, "db", value)
	txn, err := BeginSecretTxn(txnTestAppID, "container-secret-test", "test", []secrets.Binding{{Secret: "db", Env: "DB"}}, RuntimeDocker)
	if err != nil {
		t.Fatal(err)
	}
	defer txn.Set.Close()
	proc, port, err := DockerLauncherForApp(txn.AppName, "")(ctx, image, txn.Set.Overlay())
	if err != nil {
		t.Fatal(err)
	}
	containerID := containerIDOf(proc)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", containerID).Run() })
	out, err := execDockerRunner(ctx, "inspect", "--format", "{{json .Config.Env}}", containerID)
	if err != nil {
		t.Fatal(err)
	}
	var received []string
	if err := json.Unmarshal([]byte(out), &received); err != nil {
		t.Fatal(err)
	}
	matched := false
	for _, entry := range received {
		matched = matched || entry == "DB="+value
	}
	if !matched {
		t.Fatal("container changed multiline credential bytes")
	}
	path := instanceLogPath(os.Getenv("HOME"), image, port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "container-private") {
			t.Fatal("plaintext reached Phelix container log")
		}
		if strings.Count(string(data), "***") >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("container log drain did not flush")
		}
		time.Sleep(20 * time.Millisecond)
	}
	state := &DeployState{AppName: txn.AppName, AppID: txnTestAppID, Mode: ModeRolling, Runtime: RuntimeDocker,
		Replicas: map[string]*Instance{"0": {Slot: "0", ContainerID: containerID, Port: port, BinaryPath: image, Status: "running", SecretEnvs: []string{"DB"}}}}
	txn.Stamp(state)
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	if outcome, err := ReconcileSecretTxn(txn.AppName, txnTestAppID); err != nil || outcome != SecretTxnCommitted {
		t.Fatal("Docker acceptance recovery failed", outcome, err)
	}
	set, err := RuntimeSecrets(state.Replicas["0"], nil)
	if err != nil {
		t.Fatal(err)
	}
	if set.Overlay()[0] != "DB="+value {
		t.Fatal("accepted container credentials differ from committed bindings")
	}
	set.Close()
	txn.Set.Close()
	if phelixerr.Redact(value) == value {
		t.Fatal("container lost instance-owned redaction after transaction closed")
	}
	report, err := GracefulStop(ctx, proc, 3*time.Second, 0)
	if err != nil || !report.Exited {
		t.Fatal("container stop not confirmed", err)
	}
}
