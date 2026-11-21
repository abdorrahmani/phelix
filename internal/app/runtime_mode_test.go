package app

import (
	"os"
	"path/filepath"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestNativeLaunchRequiresHostMode(t *testing.T) {
	for _, mode := range []string{"", "host"} {
		t.Setenv("PHELIX_CONTROLLER_MODE", mode)
		if err := ValidateNativeRuntime(); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PHELIX_CONTROLLER_MODE", "container")
	manager := &AppManager{}
	log := filepath.Join(t.TempDir(), "native.log")
	err := manager.startApplicationProcess("app", "app", 8080, log)
	if phelixerr.CodeOf(err) != phelixerr.CodeConfiguration {
		t.Fatalf("unexpected native launch result: %v", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("rejected native launch changed the filesystem", err)
	}
}
