package controller

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceConfigurationRejectsAmbiguousRoots(t *testing.T) {
	for _, roots := range []string{
		"  - {host: /host, controller: /projects}\n  - {host: /host/nested, controller: /other}\n",
		"  - {host: /host, controller: /projects}\n  - {host: /other, controller: /projects/nested}\n",
		"  - {host: /host, controller: /projects}\n  - {host: /projects, controller: /other}\n",
	} {
		path := filepath.Join(t.TempDir(), "controller.yaml")
		if err := os.WriteFile(path, []byte("sources:\n"+roots), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil {
			t.Fatal("ambiguous roots were accepted", roots)
		}
	}
}
