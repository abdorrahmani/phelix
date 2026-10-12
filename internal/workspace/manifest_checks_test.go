package workspace

import (
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestParseManifestStrictRejectsUnknownKey(t *testing.T) {
	for _, src := range []string{
		"version: 1\nservies: {}\n",                        // top-level typo
		"version: 1\nservices:\n  auth:\n    stratgy: x\n", // nested typo
	} {
		if _, err := ParseManifest([]byte(src)); phelixerr.CodeOf(err) != phelixerr.CodeValidation {
			t.Errorf("src %q: code = %q, want VALIDATION_ERROR", src, phelixerr.CodeOf(err))
		}
	}
}

func TestParseManifestVersion(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr bool
	}{
		{"missing", "name: x\n", true},
		{"unsupported", "version: 2\n", true},
		{"ok", "version: 1\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(tt.src))
			switch {
			case tt.wantErr && phelixerr.CodeOf(err) != phelixerr.CodeValidation:
				t.Fatalf("code = %q, want VALIDATION_ERROR", phelixerr.CodeOf(err))
			case !tt.wantErr && err != nil:
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestParseManifestEnums(t *testing.T) {
	bad := []string{
		"version: 1\ndefaults: { strategy: bogus }\n",
		"version: 1\ndefaults: { runtime: podman }\n",
		"version: 1\ndiscovery: { go_work: maybe }\n",
		"version: 1\nservices:\n  a: { strategy: nope }\n",
	}
	for _, src := range bad {
		if _, err := ParseManifest([]byte(src)); phelixerr.CodeOf(err) != phelixerr.CodeValidation {
			t.Errorf("src %q: code = %q, want VALIDATION_ERROR", src, phelixerr.CodeOf(err))
		}
	}
	// Absent optional enums are fine.
	if _, err := ParseManifest([]byte("version: 1\nservices:\n  a: { port: 1 }\n")); err != nil {
		t.Errorf("absent optionals rejected: %v", err)
	}
}

func TestParseManifestEmptyServicesOK(t *testing.T) {
	for _, src := range []string{"version: 1\n", "version: 1\nservices:\n"} {
		m, err := ParseManifest([]byte(src))
		if err != nil {
			t.Fatalf("src %q: %v", src, err)
		}
		if len(m.Services) != 0 {
			t.Errorf("src %q: services = %d, want 0", src, len(m.Services))
		}
	}
}

func TestParseManifestEmptyAndMalformed(t *testing.T) {
	for _, src := range []string{"", "   \n", "::::\n", "version: 1\nname: [unclosed\n"} {
		if _, err := ParseManifest([]byte(src)); phelixerr.CodeOf(err) != phelixerr.CodeValidation {
			t.Errorf("src %q: code = %q, want VALIDATION_ERROR", src, phelixerr.CodeOf(err))
		}
	}
}

func TestParseManifestDuplicateServiceKeyRejected(t *testing.T) {
	src := "version: 1\nservices:\n  auth: { port: 1 }\n  auth: { port: 2 }\n"
	if _, err := ParseManifest([]byte(src)); phelixerr.CodeOf(err) != phelixerr.CodeValidation {
		t.Errorf("duplicate key code = %q, want VALIDATION_ERROR", phelixerr.CodeOf(err))
	}
}
