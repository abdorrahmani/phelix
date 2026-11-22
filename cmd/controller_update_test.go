package cmd

import (
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestContainerControllerUpgradeUsesImage(t *testing.T) {
	t.Setenv("PHELIX_CONTROLLER_MODE", "container")
	original := updateCheckOnly
	t.Cleanup(func() { updateCheckOnly = original })
	updateCheckOnly = false
	if err := runUpdate(); !phelixerr.IsCode(err, phelixerr.CodeConfiguration) {
		t.Fatal(err)
	}
}
