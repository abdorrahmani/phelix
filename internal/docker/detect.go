package docker

import (
	"fmt"
	"os"
	"path/filepath"
)

// Language represents the detected project language for dockerization.
type Language string

const (
	LanguageGo   Language = "go"
	LanguageRust Language = "rust"
)

// DetectLanguage checks projectRoot for go.mod (Go) or Cargo.toml (Rust).
// It returns an error if neither or both are found.
func DetectLanguage(projectRoot string) (Language, error) {
	_, hasGoMod := os.Stat(filepath.Join(projectRoot, "go.mod"))
	_, hasCargo := os.Stat(filepath.Join(projectRoot, "Cargo.toml"))

	goFound := hasGoMod == nil
	rustFound := hasCargo == nil

	switch {
	case goFound && rustFound:
		return "", fmt.Errorf("ambiguous project: both go.mod and Cargo.toml found in %s", projectRoot)
	case goFound:
		return LanguageGo, nil
	case rustFound:
		return LanguageRust, nil
	default:
		return "", fmt.Errorf("unsupported project: neither go.mod nor Cargo.toml found in %s", projectRoot)
	}
}
