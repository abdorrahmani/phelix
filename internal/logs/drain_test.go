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

func TestDetachedDrainPreservesInvalidUTF8(t *testing.T) {
	value := "opaque-\xff-byte"
	release := phelixerr.AcquireSecretValues([]string{value})
	defer release()
	path := filepath.Join(t.TempDir(), "app.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command("sh", "-c", "printf '%s\\n' \"$CREDENTIAL\"")
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
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "opaque") {
			t.Fatal("plaintext reached disk")
		}
		if strings.Contains(string(data), "***") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("drain did not flush")
}

func TestDockerDrainReconnectsAndMasksBothStreams(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PHELIX_DRAIN_TEST_DIR", dir)
	fake := `#!/bin/sh
if [ "$1" = logs ]; then
    printf '%s\n' "$*" >> "$PHELIX_DRAIN_TEST_DIR/args"
    if [ ! -f "$PHELIX_DRAIN_TEST_DIR/seen" ]; then
        touch "$PHELIX_DRAIN_TEST_DIR/seen"
        printf '%s\n' '2026-11-05T00:00:00.000000000Z first private-reconnect-value'
        printf '%s\n' '2026-11-05T00:00:00.000000000Z warning private-reconnect-value' >&2
        printf '%s' '2026-11-05T00:00:01.000000000Z private-reconnect-'
        exit 1
    fi
    printf '%s\n' '2026-11-05T00:00:02.000000000Z second private-reconnect-value' >&2
    touch "$PHELIX_DRAIN_TEST_DIR/done"
    exit 0
fi
if [ ! -f "$PHELIX_DRAIN_TEST_DIR/done" ]; then
    printf '%s\n' 'Cannot connect to the Docker daemon' >&2
    exit 1
fi
printf '%s\n' false
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, "app.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	release := phelixerr.AcquireSecretValues([]string{"private-reconnect-value"})
	defer release()
	if code := runDockerDrain("fixture-container", file, nil); code != 0 {
		t.Fatal("container drain failed", code)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil || strings.Contains(string(data), "private-reconnect-") || !strings.Contains(string(data), "first ***") || !strings.Contains(string(data), "second ***") {
		t.Fatal("reconnected output was missing or exposed credentials", err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil || !strings.Contains(string(args), "--since 2026-11-04T23:59:59Z") {
		t.Fatal("reconnection did not resume from recorded output", err)
	}
}
