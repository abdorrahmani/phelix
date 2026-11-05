package logs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestDetachedDrainMasksSplitAndMultilineValues(t *testing.T) {
	value := "opaque-first-line\nopaque-second-line"
	phelixerr.RegisterSecretValue(value)
	defer phelixerr.UnregisterSecretValue(value)
	path := filepath.Join(t.TempDir(), "app.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command("sh", "-c", "printf '%s' opaque-; sleep 0.05; printf '%s\\n' first-line; printf '%s' opaque-second-line >&2")
	cmd.Env = append(os.Environ(), "CREDENTIAL="+value)
	closeDrain, err := AttachDrain(cmd, f)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		closeDrain()
		t.Fatal(err)
	}
	closeDrain()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(data), "***") == 2 {
			if strings.Contains(string(data), "opaque") {
				t.Fatal("plaintext reached disk")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("drain did not flush both streams: %q", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
