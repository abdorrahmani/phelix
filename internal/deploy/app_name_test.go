package deploy

import (
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// An app name becomes a directory component; a name carrying a separator or
// ".." must never be turned into a path.
func TestValidateAppName(t *testing.T) {
	valid := []string{"api", "my-app", "my_app", "app.v2", "a", "9lives"}
	for _, name := range valid {
		if err := ValidateAppName(name); err != nil {
			t.Errorf("ValidateAppName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",
		"..",
		".",
		".hidden",
		"../foo",
		"a/b",
		`a\b`,
		"/abs",
		"my app",
		strings.Repeat("a", 65),
	}
	for _, name := range invalid {
		err := ValidateAppName(name)
		if err == nil {
			t.Errorf("ValidateAppName(%q) = nil, want error", name)
			continue
		}
		if !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
			t.Errorf("ValidateAppName(%q) code = %s, want INVALID_ARGUMENT", name, phelixerr.CodeOf(err))
		}
	}
}

// The constructors must refuse on their own: callers that never run the CLI
// validator (gRPC paths) reach these directly.
func TestAppDataDirRefusesUnsafeName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := appDataDir("../escape"); err == nil {
		t.Fatal("appDataDir accepted a name that escapes the data directory")
	}
	if _, err := statePath("a/b"); err == nil {
		t.Fatal("statePath accepted a name with a path separator")
	}
}
