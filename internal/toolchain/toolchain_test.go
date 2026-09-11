package toolchain

import (
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/builder"
)

func TestIsInstalled_SupportedLanguages(t *testing.T) {
	// On any host running this test, the "go" binary must be present (we are
	// building a Go project), so IsInstalled(Go) must be true. "cargo" may or
	// may not be present, so we don't assert on Rust.
	if !IsInstalled(builder.Go) {
		t.Errorf("IsInstalled(Go) = false; expected true on a host with go installed")
	}
}

func TestIsInstalled_UnknownLanguage(t *testing.T) {
	if IsInstalled(builder.Language("unknown")) {
		t.Errorf("IsInstalled(unknown) = true; expected false for unsupported language")
	}
}

func TestEnsureTool_AlreadyInstalled_ReturnsNilWithoutPrompt(t *testing.T) {
	// Go is installed on the test host, so EnsureTool should return nil and
	// must NOT call the prompt function.
	promptCalled := false
	promptFn := func(msg string) bool {
		promptCalled = true
		return false
	}

	if err := EnsureTool(builder.Go, promptFn); err != nil {
		t.Errorf("EnsureTool(Go) returned error when tool is installed: %v", err)
	}
	if promptCalled {
		t.Errorf("EnsureTool(Go) called promptFn even though tool is installed")
	}
}

func TestEnsureTool_DeclinedPrompt_ReturnsErrorWithManualInstructions(t *testing.T) {
	// Use a language whose binary is effectively never present so the missing
	// path is exercised. We craft a prompt that always declines.
	declined := func(msg string) bool { return false }

	// Pick Rust here: we cannot guarantee cargo is absent, so first check.
	// If cargo happens to be present on the CI host, skip this case rather
	// than give a false failure.
	if IsInstalled(builder.Rust) {
		t.Skip("cargo is installed on this host; skipping declined-prompt path")
	}

	err := EnsureTool(builder.Rust, declined)
	if err == nil {
		t.Fatalf("EnsureTool(Rust) with declined prompt returned nil; expected error")
	}

	// The error message must include manual instructions so the user knows
	// how to install Rust manually.
	if !strings.Contains(err.Error(), "rustup") {
		t.Errorf("expected error to contain manual rustup instructions, got: %v", err)
	}
}

func TestEnsureTool_UnsupportedLanguage_DeclinesWithoutInstall(t *testing.T) {
	declined := func(msg string) bool { return false }

	err := EnsureTool(builder.Language("ruby"), declined)
	if err == nil {
		t.Fatalf("EnsureTool(unsupported) returned nil; expected error")
	}
}

func TestManualInstructions_GoContainsExpectedURL(t *testing.T) {
	for _, lang := range []builder.Language{builder.Go, builder.Rust} {
		got := ManualInstructions(lang)
		if got == "" {
			t.Errorf("ManualInstructions(%s) returned empty string", lang)
		}
	}
}

func TestManualInstructions_GoHasGoDevLink(t *testing.T) {
	got := ManualInstructions(builder.Go)
	if !strings.Contains(got, "go.dev") {
		t.Errorf("Go manual instructions missing go.dev link: %q", got)
	}
}

func TestManualInstructions_RustHasRustupLink(t *testing.T) {
	got := ManualInstructions(builder.Rust)
	if !strings.Contains(got, "rustup") {
		t.Errorf("Rust manual instructions missing rustup reference: %q", got)
	}
}

func TestManualInstructions_UnknownLanguage(t *testing.T) {
	got := ManualInstructions(builder.Language("perl"))
	if got == "" {
		t.Errorf("ManualInstructions(unknown) returned empty string")
	}
}

func TestGoArch_KnownArchitectures(t *testing.T) {
	cases := map[string]string{
		"amd64": "amd64",
		"arm64": "arm64",
		"386":   "386",
		"mips":  "", // unsupported
	}
	for in, want := range cases {
		if got := goArch(in); got != want {
			t.Errorf("goArch(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestDetectPackageManager(t *testing.T) {
	// We can't predict which PM is available on the test host, but the
	// function must return either a valid (binary, args, true) or ("", nil, false).
	bin, args, ok := detectPackageManager("install", []string{"go", "golang"})
	if ok {
		if bin == "" {
			t.Errorf("detectPackageManager returned ok=true but empty binary")
		}
		if len(args) == 0 {
			t.Errorf("detectPackageManager returned ok=true but empty args")
		}
		return
	}
	if bin != "" {
		t.Errorf("detectPackageManager returned ok=false but non-empty binary %q", bin)
	}
}

func TestIsPermissionError(t *testing.T) {
	cases := map[string]bool{
		"permission denied":       true,
		"Operation not permitted": true,
		"Access is denied.":       true,
		"some other error":        false,
		"":                        false,
	}
	for in, want := range cases {
		err := errStr(in)
		if got := isPermissionError(err); got != want {
			t.Errorf("isPermissionError(%q) = %v; want %v", in, got, want)
		}
	}
}

// errStr is a tiny error wrapper for table-driven tests.
type strError string

func (e strError) Error() string { return string(e) }

func errStr(s string) error { return strError(s) }
