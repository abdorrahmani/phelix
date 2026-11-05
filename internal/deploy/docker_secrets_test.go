package deploy

import (
	"context"
	"strings"
	"testing"
)

func TestDockerMultilineUsesInheritedValues(t *testing.T) {
	value := "first-line\nINJECTED=second-line\n"
	run := func(ctx context.Context, args ...string) (string, error) {
		if args[0] != "run" {
			return "43210", nil
		}
		if strings.Contains(strings.Join(args, " "), value) {
			t.Fatal("value reached argv")
		}
		if strings.Contains(strings.Join(args, " "), "--env-file") {
			t.Fatal("multiline value sent through line format")
		}
		env, ok := ctx.Value(dockerEnvKey{}).([]string)
		if !ok {
			t.Fatal("container environment missing")
		}
		if len(env) != 2 || env[0] != "CREDENTIAL="+value || env[1] != "PORT=8080" {
			t.Fatal("environment changed")
		}
		return "container", nil
	}
	_, _, err := newDockerLauncher("multiline", "", "", run)(context.Background(), "image", []string{"CREDENTIAL=" + value})
	if err != nil {
		t.Fatal(err)
	}
}
