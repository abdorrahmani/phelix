package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
)

func TestCLISecretClassicLifecycle(t *testing.T) {
	root := t.TempDir()
	home, projectDir := filepath.Join(root, "home"), filepath.Join(root, "project")
	dataDir := filepath.Join(home, ".phelix")
	for _, dir := range []string{dataDir, projectDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	const id = "50102030-4050-4670-8090-aabbccddeeff"
	const name = "secret-classic"
	const oldValue = "original-private-credential-449"
	const newValue = "rotated-private-credential-719"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	state, err := json.Marshal(map[string]any{id: map[string]any{
		"id": id, "name": name, "directory": projectDir, "status": "stopped", "port": port, "language": "go",
	}})
	if err != nil {
		t.Fatal(err)
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dataDir, "apps.json"), string(state))
	write(filepath.Join(projectDir, "phelix.yaml"), "name: "+name+"\nsecrets:\n  imports:\n    - secret: db\n      env: DB\n")
	write(filepath.Join(projectDir, "go.mod"), "module example.com/secretfixture\n\ngo 1.22\n")
	cli := phelixBin(t)
	run := func(input string, args ...string) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, cli, args...)
		command.Dir = projectDir
		command.Env = append(os.Environ(), "HOME="+home, "PHELIX_DATA_DIR="+dataDir, "GOCACHE="+filepath.Join(root, "cache"))
		command.Stdin = strings.NewReader(input)
		output, err := command.CombinedOutput()
		for _, value := range []string{oldValue, newValue} {
			if strings.Contains(string(output), value) {
				t.Fatal("CLI output exposed a credential")
			}
		}
		if err != nil {
			t.Logf("command %v: %v\n%s", args, err, output)
		}
		return err
	}
	mustRun := func(input string, args ...string) {
		t.Helper()
		if err := run(input, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = run("", "stop", name) })
	source := func(version, failCondition string) {
		write(filepath.Join(projectDir, "main.go"), fmt.Sprintf(`package main
import ("fmt"; "net/http"; "os")
func main() {
if %s { os.Exit(1) }
fmt.Println(os.Getenv("DB"))
http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, %q+"|"+os.Getenv("DB")) })
http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), nil)
}
`, failCondition, version))
	}
	assertServing := func(want string) {
		t.Helper()
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil || string(data) != want {
			t.Fatal("running version or credentials differ from expected state", err)
		}
	}
	mustRun(oldValue, "secrets", "set", "db", "--no-apply")
	source("v1", "false")
	mustRun("", "rebuild", name)
	assertServing("v1|" + oldValue)
	mustRun(newValue, "secrets", "set", "db", "--no-apply")
	write(filepath.Join(projectDir, "main.go"), "package main\nfunc main() { broken }\n")
	if err := run("", "rebuild", name); err == nil {
		t.Fatal("invalid build succeeded")
	}
	assertServing("v1|" + oldValue)
	source("failed", "true")
	if err := run("", "rebuild", name); err == nil {
		t.Fatal("failed startup succeeded")
	}
	assertServing("v1|" + oldValue)
	source("v2", "false")
	mustRun("", "rebuild", name)
	assertServing("v2|" + newValue)
	mustRun("", "rollback", name, "--to", "v1")
	assertServing("v1|" + newValue)
	journal, err := os.ReadFile(filepath.Join(dataDir, "apps", name, "deploy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var deployed deploy.DeployState
	if err := json.Unmarshal(journal, &deployed); err != nil {
		t.Fatal(err)
	}
	if deployed.SecretTxnID != "" || deployed.ActiveVersion != 1 || deployed.Slots["classic"] == nil || deployed.Slots["classic"].PID == 0 {
		t.Fatal("rollback left stale acceptance or process records")
	}
	mustRun("", "secrets", "get", "db", "--json")
	mustRun("", "secrets", "history", "db", "--json")
	mustRun("", "secrets", "list", "--json")
	mustRun("", "stop", name)
	if err := filepath.WalkDir(dataDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return err
		}
		if strings.Contains(path, string(filepath.Separator)+"builds"+string(filepath.Separator)) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, value := range []string{oldValue, newValue} {
			if strings.Contains(string(data), value) {
				return fmt.Errorf("credential reached managed file %s", path)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
