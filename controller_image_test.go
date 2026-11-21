package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestControllerReleaseTagsAndValidationGates(t *testing.T) {
	command := exec.Command("bash", "-c", `source .github/scripts/release-tag.sh; read_release_tag "$1"; printf '%s' "$IMAGE_TAG"`, "_", "v1.3.0-beta.1+build.7")
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "1.3.0-beta.1_build.7" {
		t.Fatal(string(output), err)
	}
	raw, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Needs any `yaml:"needs"`
			Steps []struct {
				Name string `yaml:"name"`
				If   string `yaml:"if"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	image := workflow.Jobs["controller-image"]
	checked := false
	for _, step := range image.Steps {
		if strings.HasPrefix(step.Name, "Check startup") {
			checked = true
		}
		if step.Name == "Publish validated architecture" && (!checked || step.If != "github.event_name == 'push' || inputs.publish") {
			t.Fatal("image publication is not gated by validation and release intent")
		}
	}
	if !checked || len(workflow.Jobs["controller-manifest"].Needs.([]any)) != 2 {
		t.Fatal("architecture validation or manifest gate is missing")
	}
}
