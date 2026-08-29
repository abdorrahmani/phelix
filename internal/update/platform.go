package update

import (
	"runtime"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Platform identifies a release artifact in the installer's naming scheme:
// phelix-<os>-<arch> with arch ∈ {amd64, arm64, armv7, armv6}.
type Platform struct {
	OS   string
	Arch string
}

// String renders "os/arch" using the release artifact names.
func (p Platform) String() string { return p.OS + "/" + p.Arch }

// DetectPlatform maps the running platform onto the release artifact naming
// used by install.sh and release.sh. GOARCH "arm" is ambiguous (GOARM 6 or 7)
// and is resolved from the kernel machine name, which is what install.sh's
// `uname -m` sees.
func DetectPlatform() (Platform, error) {
	return detectPlatform(runtime.GOOS, runtime.GOARCH, unameMachine)
}

// detectPlatform is the injectable core of DetectPlatform.
func detectPlatform(goos, goarch string, machine func() (string, error)) (Platform, error) {
	switch goos {
	case "linux", "darwin":
		// Supported: full support (binary + systemd) on Linux, binary-only
		// on macOS, mirroring install.sh.
	default:
		return Platform{}, phelixerr.Newf(phelixerr.CodeConfiguration,
			"phelix update is not supported on %s; supported platforms: linux (amd64, arm64, armv7, armv6) and darwin (amd64, arm64)", goos)
	}

	switch goarch {
	case "amd64", "arm64":
		return Platform{OS: goos, Arch: goarch}, nil
	case "arm":
		// Only 32-bit Linux maps to the armv6/armv7 artifacts; 64-bit ARM is
		// GOARCH arm64 above.
		m, err := machine()
		if err != nil {
			return Platform{}, phelixerr.Wrapf(phelixerr.CodeConfiguration, err,
				"cannot determine the ARM variant of this machine")
		}
		switch m {
		case "armv6l", "armv6":
			return Platform{OS: goos, Arch: "armv6"}, nil
		case "armv7l", "armv7":
			return Platform{OS: goos, Arch: "armv7"}, nil
		default:
			return Platform{}, phelixerr.Newf(phelixerr.CodeConfiguration,
				"unsupported CPU architecture: %s; supported: amd64, arm64, armv7, armv6", m)
		}
	}
	return Platform{}, phelixerr.Newf(phelixerr.CodeConfiguration,
		"unsupported CPU architecture: %s; supported: amd64, arm64, armv7, armv6", goarch)
}
