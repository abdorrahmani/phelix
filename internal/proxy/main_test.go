package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain repoints the data dir so tests that exercise the access middleware
// (and any other code that writes under the data dir) stay hermetic instead
// of appending to the developer's real ~/.phelix.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "phelix-proxy-test")
	if err == nil {
		_ = os.Setenv("PHELIX_DATA_DIR", filepath.Join(dir, ".phelix"))
		defer os.RemoveAll(dir)
	}
	os.Exit(m.Run())
}
