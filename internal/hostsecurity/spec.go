package hostsecurity

import (
	"errors"
	"regexp"
	"strings"
)

// packageSpecPattern matches an update target: a package name optionally
// pinned to an exact version with '='.
var packageSpecPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]*(=[a-zA-Z0-9][a-zA-Z0-9._+:~^-]*)?$`)

// MaxUpdatePackages bounds the package list of one update operation.
const MaxUpdatePackages = 50

// ValidatePackageSpec validates one update target. The name part must be a
// valid package name; an optional =version pins the exact target version.
func ValidatePackageSpec(spec string) error {
	if spec == "" || len(spec) > 256 {
		return errors.New("package specification must be 1..256 characters")
	}
	if !packageSpecPattern.MatchString(spec) {
		return errors.New("package specification must be a package name optionally pinned with =version")
	}
	name, _, _ := strings.Cut(spec, "=")
	if !ValidPackageName(name) {
		return errors.New("package name is not a valid package identity")
	}
	return nil
}

// SplitPackageSpec separates a validated spec into its name and optional
// pinned version.
func SplitPackageSpec(spec string) (string, string) {
	name, version, _ := strings.Cut(spec, "=")
	return name, version
}
