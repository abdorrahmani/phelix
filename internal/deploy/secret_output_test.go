package deploy

import (
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"strings"
	"testing"
)

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
