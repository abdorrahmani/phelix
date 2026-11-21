package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/project"
)

func TestDockerLauncherNamedVolume(t *testing.T) {
	volume := project.DockerVolume{Name: "b-phelix_phelix-attachments", Target: "/data/ticket-attachments"}
	fd := &fakeDocker{run: "test-container", inspect: "49215\n"}
	launcher := newDockerLauncher("ticket", "blue", "", fd.runner(), volume)
	if _, _, err := launcher(context.Background(), "ticket:v1", nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fd.calls[0], " "); got != "volume inspect b-phelix_phelix-attachments" {
		t.Fatalf("volume check = %q", got)
	}
	if got := strings.Join(fd.calls[1], " "); !strings.Contains(got, "--mount type=volume,source=b-phelix_phelix-attachments,target=/data/ticket-attachments") {
		t.Fatalf("mount missing from %q", got)
	}
}

func TestDockerLauncherMissingVolume(t *testing.T) {
	fd := &fakeDocker{errs: map[string]error{"volume": errors.New("missing volume")}}
	launcher := newDockerLauncher("ticket", "blue", "", fd.runner(), project.DockerVolume{Name: "attachments", Target: "/data"})
	if _, _, err := launcher(context.Background(), "ticket:v1", nil); err == nil {
		t.Fatal("missing volume was accepted")
	}
	if len(fd.calls) != 1 || fd.calls[0][0] != "volume" {
		t.Fatalf("container launched after failed validation: %v", fd.calls)
	}
}
