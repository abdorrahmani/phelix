package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerChannels(t *testing.T) {
	for _, tc := range []struct {
		name, channel, version, path, header string
		args                                 []string
		fail, missing, badChecksum, wget     bool
	}{
		{name: "stable default", version: "1.2.3", path: "/releases/latest/version"},
		{name: "beta body", version: "1.3.0-beta.1", path: "/releases/beta/version", args: []string{"--channel", "beta"}},
		{name: "beta header", version: "1.3.0-beta.2", header: "v1.3.0-beta.2", path: "/releases/beta/version", args: []string{"--channel", "beta"}},
		{name: "beta environment", channel: "beta", version: "1.3.0-beta.1", path: "/releases/beta/version"},
		{name: "flag overrides environment", channel: "beta", version: "1.2.3", path: "/releases/latest/version", args: []string{"--channel", "stable"}},
		{name: "pinned beta", version: "1.3.0-beta.1", args: []string{"--version", "v1.3.0-beta.1"}},
		{name: "beta via wget", version: "1.3.0-beta.1", path: "/releases/beta/version", args: []string{"--channel", "beta"}, wget: true},
		{name: "missing beta", path: "/releases/beta/version", args: []string{"--channel", "beta"}, missing: true, fail: true},
		{name: "stable rejects prerelease", version: "1.3.0-beta.1", path: "/releases/latest/version", fail: true},
		{name: "beta rejects stable", version: "1.3.0", path: "/releases/beta/version", args: []string{"--channel", "beta"}, fail: true},
		{name: "beta rejects malformed metadata", version: "../latest", path: "/releases/beta/version", args: []string{"--channel", "beta"}, fail: true},
		{name: "invalid channel", args: []string{"--channel", "nightly"}, fail: true},
		{name: "invalid pinned version", args: []string{"--version", "../latest"}, fail: true},
		{name: "checksum mismatch", version: "1.3.0-beta.1", path: "/releases/beta/version", args: []string{"--channel", "beta"}, badChecksum: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte("#!/bin/sh\nprintf 'Phelix beta\\n'\n")
			artifact := "/releases/" + tc.version + "/phelix-linux-amd64"
			downloads := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case tc.path:
					if tc.missing {
						http.NotFound(w, r)
						return
					}
					if tc.header != "" {
						w.Header().Set("x-phelix-version", tc.header)
						fmt.Fprintln(w, "1.2.3")
						return
					}
					fmt.Fprintln(w, tc.version)
				case artifact:
					downloads++
					w.Write(payload)
				case artifact + ".sha256":
					sum := fmt.Sprintf("%x", sha256.Sum256(payload))
					if tc.badChecksum {
						sum = strings.Repeat("0", 64)
					}
					fmt.Fprintf(w, "%s  phelix-linux-amd64\n", sum)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			t.Setenv("PHELIX_CHANNEL", tc.channel)
			t.Setenv("PHELIX_VERSION", "")
			t.Setenv("PHELIX_RELEASE_BASE_URL", srv.URL)
			dir := t.TempDir()
			target := filepath.Join(dir, "phelix")
			if err := os.WriteFile(target, []byte("previous binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			output, err := runInstaller(t, dir, tc.wget, tc.args...)
			if (err != nil) != tc.fail {
				t.Fatalf("installer error = %v, want failure %v; output:\n%s", err, tc.fail, output)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			want := payload
			if tc.fail {
				want = []byte("previous binary")
			}
			if string(got) != string(want) {
				t.Fatalf("installed contents = %q, want %q", got, want)
			}
			if tc.fail && !tc.badChecksum && downloads != 0 {
				t.Fatal("invalid release selection downloaded a binary")
			}
		})
	}
}

func TestInstallerRejectsInsecureMirror(t *testing.T) {
	t.Setenv("PHELIX_RELEASE_BASE_URL", "http://example.com")
	output, err := runInstaller(t, t.TempDir(), false)
	if err == nil || !strings.Contains(output, "must use HTTPS") {
		t.Fatalf("error = %v, output = %s", err, output)
	}
}

func runInstaller(t *testing.T, dir string, wget bool, args ...string) (string, error) {
	t.Helper()
	binDir := t.TempDir()
	commands := []string{"bash", "mktemp", "rm", "install", "mkdir", "awk", "sed", "cat", "sha256sum"}
	if wget {
		commands = append(commands, "wget")
	} else {
		commands = append(commands, "curl")
	}
	for _, name := range commands {
		path, err := exec.LookPath(name)
		if err != nil {
			if wget && name == "wget" {
				t.Skip("wget is unavailable")
			}
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(binDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for name, script := range map[string]string{
		"id":    "#!/bin/sh\nprintf '0\\n'\n",
		"uname": "#!/bin/sh\ncase \"$1\" in -s) printf 'Linux\\n';; -m) printf 'x86_64\\n';; esac\n",
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	args = append([]string{"install.sh", "--install-dir", dir, "--no-service"}, args...)
	command := exec.Command(filepath.Join(binDir, "bash"), args...)
	command.Env = append(os.Environ(), "PATH="+binDir)
	output, err := command.CombinedOutput()
	return string(output), err
}
