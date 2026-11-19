package main

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestGitHubReleaseTags(t *testing.T) {
	for _, tc := range []struct {
		tag, want string
		fail      bool
	}{
		{"v1.3.0", "1.3.0 false", false},
		{"v1.3.0-beta.1", "1.3.0-beta.1 true", false},
		{"v1.3.0-beta", "1.3.0-beta true", false},
		{"v1.3.0+build.7", "1.3.0+build.7 false", false},
		{"v1.3.0-beta.2+build.7", "1.3.0-beta.2+build.7 true", false},
		{"1.3.0", "", true},
		{"v1.3.0dev", "", true},
		{"v1.3.0-rc.1", "", true},
		{"v1.3.0-beta.1;false", "", true},
		{"../latest", "", true},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			command := exec.Command("bash", "-c", `set -euo pipefail; source .github/scripts/release-tag.sh; read_release_tag "$1"; printf '%s %s' "$VERSION" "$PRERELEASE"`, "_", tc.tag)
			output, err := command.CombinedOutput()
			if (err != nil) != tc.fail || (!tc.fail && string(output) != tc.want) {
				t.Fatalf("tag = %q, output = %q, error = %v", tc.tag, output, err)
			}
		})
	}
}

func TestGitHubArtifactBuildRestoresConfig(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", fail), func(t *testing.T) {
			dir := releaseBuildFixture(t, "dev")
			original, err := os.ReadFile(filepath.Join(dir, "config/config.yml"))
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", ".github/scripts/build-artifacts.sh", "v1.3.0-beta.1", "artifacts", "42.1")
			command.Dir = dir
			command.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), fmt.Sprintf("PHELIX_TEST_BUILD_FAIL=%d", map[bool]int{false: 0, true: 1}[fail]))
			output, runErr := command.CombinedOutput()
			if (runErr != nil) != fail {
				t.Fatalf("error = %v, output = %s", runErr, output)
			}
			got, err := os.ReadFile(filepath.Join(dir, "config/config.yml"))
			if err != nil || string(got) != string(original) {
				t.Fatalf("configuration was not restored: %q, %v", got, err)
			}
			if fail {
				return
			}
			artifacts := releaseDirectoryContents(t, filepath.Join(dir, "artifacts"))
			if len(artifacts) != 14 || artifacts["version"] != "1.3.0-beta.1\n" || strings.Count(artifacts["checksums.sha256"], "\n") != 6 {
				t.Fatalf("unexpected release artifacts: %v", artifacts)
			}
			if _, err := os.Stat(filepath.Join(dir, "releases/latest")); !os.IsNotExist(err) {
				t.Fatal("GitHub artifact build changed latest")
			}
		})
	}
}

func TestGitHubPublishing(t *testing.T) {
	for _, tc := range []struct {
		name, tag, state               string
		fail, uploadFail, badChecksum  bool
		badVersion, missing, published bool
	}{
		{name: "stable", tag: "v1.3.0"},
		{name: "beta", tag: "v1.3.0-beta.1"},
		{name: "retry draft", tag: "v1.3.0-beta.1", state: "true"},
		{name: "published is immutable", tag: "v1.3.0", state: "false", fail: true, published: true},
		{name: "failed upload stays draft", tag: "v1.3.0-beta.1", uploadFail: true, fail: true},
		{name: "bad checksum", tag: "v1.3.0-beta.1", badChecksum: true, fail: true},
		{name: "wrong version", tag: "v1.3.0-beta.1", badVersion: true, fail: true},
		{name: "missing binary", tag: "v1.3.0-beta.1", missing: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := githubPublishFixture(t, tc.tag)
			if tc.badChecksum {
				if err := os.WriteFile(filepath.Join(dir, "phelix-linux-amd64"), []byte("modified"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.badVersion {
				if err := os.WriteFile(filepath.Join(dir, "version"), []byte("1.2.3\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.missing {
				if err := os.Remove(filepath.Join(dir, "phelix-linux-amd64")); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PHELIX_TEST_RELEASE_STATE", tc.state)
			t.Setenv("PHELIX_TEST_UPLOAD_FAIL", fmt.Sprint(tc.uploadFail))
			before := releaseDirectoryContents(t, dir)
			command := exec.Command("bash", ".github/scripts/publish-github.sh", tc.tag, dir)
			output, err := command.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("error = %v, output = %s", err, output)
			}
			if got := releaseDirectoryContents(t, dir); !maps.Equal(got, before) {
				t.Fatal("publishing modified the release files")
			}
			log, _ := os.ReadFile(os.Getenv("PHELIX_TEST_GH_LOG"))
			calls := strings.ReplaceAll(string(log), "\x00", " ")
			if tc.badChecksum || tc.badVersion || tc.missing {
				if calls != "" {
					t.Fatalf("invalid artifacts contacted GitHub: %s", calls)
				}
				return
			}
			if tc.published || tc.uploadFail {
				if strings.Contains(calls, "release edit") || (tc.published && strings.Contains(calls, "release upload")) {
					t.Fatalf("failed publication changed a public release: %s", calls)
				}
				return
			}
			if tc.state == "true" && strings.Contains(calls, "release create") {
				t.Fatalf("retry created a second release: %s", calls)
			}
			if tc.state == "" && (!strings.Contains(calls, "--verify-tag") || !strings.Contains(calls, "--draft")) {
				t.Fatalf("release was not created as a verified draft: %s", calls)
			}
			latest, prerelease := "true", "false"
			if strings.Contains(tc.tag, "-beta") {
				latest, prerelease = "false", "true"
			}
			if !strings.Contains(calls, "--latest="+latest) || !strings.Contains(calls, "--prerelease="+prerelease) || !strings.Contains(calls, "--draft=false") {
				t.Fatalf("wrong release status: %s", calls)
			}
			lines := strings.Split(string(log), "\n")
			for _, line := range lines {
				fields := strings.Split(line, "\x00")
				if len(fields) < 2 || fields[1] != "upload" {
					continue
				}
				assets := 0
				for _, field := range fields {
					if strings.HasPrefix(field, dir+"/") {
						assets++
					}
				}
				if assets != 14 {
					t.Fatalf("uploaded %d assets, want 14", assets)
				}
			}
		})
	}
}

func githubPublishFixture(t *testing.T, tag string) string {
	t.Helper()
	dir := t.TempDir()
	var manifest strings.Builder
	for _, name := range []string{"phelix-linux-amd64", "phelix-linux-arm64", "phelix-linux-armv7", "phelix-linux-armv6", "phelix-darwin-amd64", "phelix-darwin-arm64"} {
		payload := []byte("Phelix " + name)
		checksum := fmt.Sprintf("%x  %s\n", sha256.Sum256(payload), name)
		for path, contents := range map[string][]byte{name: payload, name + ".sha256": []byte(checksum)} {
			if err := os.WriteFile(filepath.Join(dir, path), contents, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		manifest.WriteString(checksum)
	}
	for name, contents := range map[string]string{"checksums.sha256": manifest.String(), "version": strings.TrimPrefix(tag, "v") + "\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	binDir := t.TempDir()
	stub := `#!/usr/bin/env bash
printf '%s\0' "$@" >> "$PHELIX_TEST_GH_LOG"
printf '\n' >> "$PHELIX_TEST_GH_LOG"
case "$2" in
    view)
        test -n "$PHELIX_TEST_RELEASE_STATE" || exit 1
        printf '%s\n' "$PHELIX_TEST_RELEASE_STATE"
        ;;
    upload)
        test "$PHELIX_TEST_UPLOAD_FAIL" != true
        ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("PHELIX_TEST_GH_LOG", filepath.Join(t.TempDir(), "calls"))
	t.Setenv("GH_REPO", "abdorrahmani/phelix")
	return dir
}

func TestGitHubReleaseWorkflow(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On struct {
			Push struct {
				Tags []string `yaml:"tags"`
			} `yaml:"push"`
			Dispatch struct {
				Inputs map[string]struct {
					Default bool `yaml:"default"`
				} `yaml:"inputs"`
			} `yaml:"workflow_dispatch"`
		} `yaml:"on"`
		Jobs map[string]struct {
			RunsOn []string `yaml:"runs-on"`
			Steps  []struct {
				Name string `yaml:"name"`
				If   string `yaml:"if"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	job, ok := workflow.Jobs["release"]
	if !ok || !reflect.DeepEqual(job.RunsOn, []string{"self-hosted", "linux", "x64"}) {
		t.Fatalf("runner labels = %v", job.RunsOn)
	}
	if !reflect.DeepEqual(workflow.On.Push.Tags, []string{"v*"}) || workflow.On.Dispatch.Inputs["publish"].Default {
		t.Fatal("tag trigger or manual publication default is incorrect")
	}
	for _, step := range job.Steps {
		if step.Name == "Publish GitHub release" {
			if step.If != "github.event_name == 'push' || inputs.publish" {
				t.Fatalf("publication condition = %q", step.If)
			}
			return
		}
	}
	t.Fatal("publication step is missing")
}
