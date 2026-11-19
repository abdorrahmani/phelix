package main

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalReleaseChannels(t *testing.T) {
	t.Setenv("PHELIX_TEST_BUILD_FAIL", "")
	dir := localReleaseFixture(t, "production")
	run := func(version string, fail bool) {
		t.Helper()
		command := exec.Command("bash", "release.sh", version, "20261119.1")
		command.Dir = dir
		command.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()
		if (err != nil) != fail {
			t.Fatalf("release %s: error = %v, want failure %v; output:\n%s", version, err, fail, output)
		}
	}
	run("1.2.3", false)
	stable := releaseDirectoryContents(t, filepath.Join(dir, "releases/latest"))
	run("v1.3.0-beta.1", false)
	betaDir := filepath.Join(dir, "releases/beta")
	beta := releaseDirectoryContents(t, betaDir)
	if beta["version"] != "1.3.0-beta.1\n" || len(beta) != 14 {
		t.Fatalf("beta release has unexpected artifacts: %v", beta)
	}
	for name, contents := range beta {
		if !strings.HasPrefix(name, "phelix-") || strings.HasSuffix(name, ".sha256") {
			continue
		}
		want := fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte(contents)), name)
		if beta[name+".sha256"] != want {
			t.Fatalf("wrong checksum for %s", name)
		}
	}
	if versioned := releaseDirectoryContents(t, filepath.Join(dir, "releases/1.3.0-beta.1")); !maps.Equal(versioned, beta) {
		t.Fatal("beta channel differs from its versioned release")
	}
	run("1.3.0-beta.2+build.7", false)
	if got := releaseDirectoryContents(t, betaDir)["version"]; got != "1.3.0-beta.2+build.7\n" {
		t.Fatalf("beta version = %q", got)
	}
	beta = releaseDirectoryContents(t, betaDir)
	t.Setenv("PHELIX_TEST_BUILD_FAIL", "1")
	run("1.3.0-beta.3", true)
	t.Setenv("PHELIX_TEST_BUILD_FAIL", "")
	run("1.3.0dev", false)
	run("1.3.0-rc.1", false)
	run("../latest", true)
	run("1.3.0-", true)
	if got := releaseDirectoryContents(t, filepath.Join(dir, "releases/latest")); !maps.Equal(got, stable) {
		t.Fatal("a prerelease changed latest")
	}
	if got := releaseDirectoryContents(t, betaDir); !maps.Equal(got, beta) {
		t.Fatal("another prerelease changed beta")
	}
	run("1.3.0", false)
	if got := releaseDirectoryContents(t, filepath.Join(dir, "releases/latest"))["version"]; got != "1.3.0\n" {
		t.Fatalf("stable version = %q", got)
	}
}

func TestLocalBetaReleaseRequiresProduction(t *testing.T) {
	dir := localReleaseFixture(t, "dev")
	command := exec.Command("bash", "release.sh", "1.3.0-beta.1")
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "a release must ship mode: production") {
		t.Fatalf("error = %v, output = %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(dir, "releases")); !os.IsNotExist(err) {
		t.Fatal("failed preflight created release artifacts")
	}
}

func localReleaseFixture(t *testing.T, mode string) string {
	t.Helper()
	script, err := os.ReadFile("release.sh")
	if os.IsNotExist(err) {
		t.Skip("release.sh is local and is not included in the repository")
	}
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, name := range []string{"config", "bin"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"release.sh":        string(script),
		"config/config.yml": fmt.Sprintf("mode: %s\ngrpcUrl: phelix.anophel.com:443\n", mode),
		"bin/git":           "#!/bin/sh\nprintf 'abc1234\\n'\n",
		"bin/go": `#!/bin/sh
if [ "$1" = env ]; then
    printf '/tmp/phelix-test-toolchain\n'
    exit 0
fi
if [ "${PHELIX_TEST_BUILD_FAIL:-}" = 1 ]; then
    exit 1
fi
while [ "$#" -gt 0 ]; do
    if [ "$1" = -o ]; then
        printf 'Phelix %s/%s/%s\n' "$GOOS" "$GOARCH" "${GOARM:-}" > "$2"
        exit 0
    fi
    shift
done
exit 1
`,
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func releaseDirectoryContents(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	contents := make(map[string]string, len(entries))
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		contents[entry.Name()] = string(data)
	}
	return contents
}
