package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/project"
)

// fakeDockerCmd records docker invocations and returns scripted stdout per
// subcommand, so the compose build dispatcher and the ownership warning run
// without a Docker daemon.
type fakeDockerCmd struct {
	calls    [][]string
	config   string           // stdout for `docker compose ... config`
	ps       string           // stdout for `docker ps`
	errOnSub map[string]error // keyed by a substring that must appear in the joined args
}

func (f *fakeDockerCmd) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	for sub, err := range f.errOnSub {
		if strings.Contains(joined, sub) {
			return "", err
		}
	}
	switch {
	case strings.Contains(joined, "config"):
		return f.config, nil
	case len(args) > 0 && args[0] == "ps":
		return f.ps, nil
	default:
		return "", nil
	}
}

func (f *fakeDockerCmd) called(sub string) bool {
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), sub) {
			return true
		}
	}
	return false
}

func withFakeDocker(t *testing.T, f *fakeDockerCmd) {
	t.Helper()
	prev := dockerCmdRunner
	dockerCmdRunner = f.run
	t.Cleanup(func() { dockerCmdRunner = prev })
}

func TestComposeBuildImage_TagsAppVN(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fd := &fakeDockerCmd{config: `{"name":"proj","services":{"app":{"image":""}}}`}
	withFakeDocker(t, fd)

	ref, err := dockerImageBuilder(dir, project.DockerBuildCompose, "docker-compose.yml", "app")(
		context.Background(), "billing", 3)
	if err != nil {
		t.Fatalf("compose build: %v", err)
	}
	if ref != "billing:v3" {
		t.Fatalf("image ref = %q, want billing:v3", ref)
	}
	if !fd.called("compose -f") || !fd.called("build app") {
		t.Errorf("expected a `compose ... build app` call, got %v", fd.calls)
	}
	// A build-only service resolves to <project>-<service>; that ref must be the
	// one retagged to billing:v3.
	if !fd.called("tag proj-app billing:v3") {
		t.Errorf("expected `tag proj-app billing:v3`, got %v", fd.calls)
	}
}

func TestComposeBuildImage_MissingComposeFile(t *testing.T) {
	fd := &fakeDockerCmd{}
	withFakeDocker(t, fd)
	_, err := dockerImageBuilder(t.TempDir(), project.DockerBuildCompose, "docker-compose.yml", "app")(
		context.Background(), "billing", 1)
	if err == nil {
		t.Fatal("expected an error for a missing compose file")
	}
	if phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		t.Fatalf("code = %v, want NOT_FOUND", phelixerr.CodeOf(err))
	}
	if len(fd.calls) != 0 {
		t.Fatalf("nothing should be built when the compose file is missing, got %v", fd.calls)
	}
}

func TestComposeServiceImage(t *testing.T) {
	// Explicit image wins.
	got, err := composeServiceImage(`{"name":"proj","services":{"app":{"image":"myorg/api:tag"}}}`, "app")
	if err != nil || got != "myorg/api:tag" {
		t.Fatalf("explicit image: got %q err %v", got, err)
	}
	// Build-only service → compose default <project>-<service>.
	got, err = composeServiceImage(`{"name":"proj","services":{"app":{}}}`, "app")
	if err != nil || got != "proj-app" {
		t.Fatalf("build-only image: got %q err %v", got, err)
	}
	// Missing service → NOT_FOUND, naming the available services.
	_, err = composeServiceImage(`{"name":"proj","services":{"web":{}}}`, "app")
	if err == nil || phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		t.Fatalf("missing service: want NOT_FOUND, got %v", err)
	}
}

// recordingLogger captures Warnf output for the ownership-warning test.
type recordingLogger struct{ warnings []string }

func (l *recordingLogger) Stepf(string, ...any)     {}
func (l *recordingLogger) Infof(string, ...any)     {}
func (l *recordingLogger) Warnf(f string, a ...any) { l.warnings = append(l.warnings, f) }
func (l *recordingLogger) Successf(string, ...any)  {}
func (l *recordingLogger) Errorf(string, ...any)    {}

func TestWarnIfComposeManaged_FiresOnComposeContainer(t *testing.T) {
	fd := &fakeDockerCmd{ps: "abc123|proj-app-1|\n"} // no phelix.managed label
	withFakeDocker(t, fd)
	log := &recordingLogger{}
	warnIfComposeManaged(context.Background(), log, "app", "app")
	if len(log.warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %d: %v", len(log.warnings), log.warnings)
	}
}

func TestWarnIfComposeManaged_SilentForPhelixManaged(t *testing.T) {
	// The only matching container is one Phelix itself manages — not a conflict.
	fd := &fakeDockerCmd{ps: "abc123|app-blue|true\n"}
	withFakeDocker(t, fd)
	log := &recordingLogger{}
	warnIfComposeManaged(context.Background(), log, "app", "app")
	if len(log.warnings) != 0 {
		t.Fatalf("phelix-managed container must not warn, got %v", log.warnings)
	}
}

func TestWarnIfComposeManaged_SilentWhenNone(t *testing.T) {
	fd := &fakeDockerCmd{ps: "\n"} // no compose container running (profile pattern)
	withFakeDocker(t, fd)
	log := &recordingLogger{}
	warnIfComposeManaged(context.Background(), log, "app", "app")
	if len(log.warnings) != 0 {
		t.Fatalf("no running compose service must stay silent, got %v", log.warnings)
	}
}
