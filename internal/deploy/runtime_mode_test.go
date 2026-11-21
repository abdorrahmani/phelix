package deploy

import (
	"context"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/resources"
)

func TestNativeDeployLaunchersRejectContainerMode(t *testing.T) {
	t.Setenv("PHELIX_CONTROLLER_MODE", "container")
	if _, _, err := DefaultLauncher(context.Background(), "/missing", nil); phelixerr.CodeOf(err) != phelixerr.CodeConfiguration {
		t.Fatalf("native service launch was not rejected: %v", err)
	}
	if _, _, err := launchWorkerInstance("/missing", nil, resources.Config{}, "0"); phelixerr.CodeOf(err) != phelixerr.CodeConfiguration {
		t.Fatalf("native worker launch was not rejected: %v", err)
	}
}
