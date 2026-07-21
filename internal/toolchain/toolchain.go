package toolchain

import (
	"fmt"
	"os/exec"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// toolBinary maps each supported language to its primary CLI binary.
var toolBinary = map[builder.Language]string{
	builder.Go:   "go",
	builder.Rust: "cargo",
}

// toolName maps each language to a human-readable name for prompts/messages.
var toolName = map[builder.Language]string{
	builder.Go:   "Go toolchain (go)",
	builder.Rust: "Rust toolchain (cargo/rustup)",
}

// IsInstalled checks whether the primary binary for the given language
// is available on the current PATH.
func IsInstalled(lang builder.Language) bool {
	bin, ok := toolBinary[lang]
	if !ok {
		return false
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

// EnsureTool checks whether the toolchain for lang is installed.
// If it is already present, EnsureTool returns nil immediately.
// If it is missing, EnsureTool calls promptFn with a human-readable message.
//   - If promptFn returns false (user declined), EnsureTool returns an error
//     whose message contains OS-specific manual installation instructions.
//   - If promptFn returns true (user accepted), EnsureTool attempts to install
//     the toolchain. On success, it refreshes the process PATH and returns nil.
//     On failure, it returns an error with manual installation instructions.
func EnsureTool(lang builder.Language, promptFn func(msg string) bool) error {
	if IsInstalled(lang) {
		return nil
	}

	name := toolName[lang]
	msg := fmt.Sprintf("%s is not installed. Install it now?", name)

	if !promptFn(msg) {
		return fmt.Errorf("%s not installed and installation was declined.\n\n%s",
			name, ManualInstructions(lang))
	}

	if err := Install(lang); err != nil {
		return fmt.Errorf("failed to install %s: %v\n\n%s",
			name, err, ManualInstructions(lang))
	}

	return nil
}
