package update

import (
	"errors"
	"runtime"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestDetectPlatformTable(t *testing.T) {
	okMachine := func() (string, error) { return "x86_64", nil }

	cases := []struct {
		name    string
		goos    string
		goarch  string
		machine func() (string, error)
		want    Platform
		wantErr bool
	}{
		{"linux amd64", "linux", "amd64", okMachine, Platform{"linux", "amd64"}, false},
		{"linux arm64", "linux", "arm64", okMachine, Platform{"linux", "arm64"}, false},
		{"darwin amd64", "darwin", "amd64", okMachine, Platform{"darwin", "amd64"}, false},
		{"darwin arm64", "darwin", "arm64", okMachine, Platform{"darwin", "arm64"}, false},
		{"linux arm machine armv7l", "linux", "arm", func() (string, error) { return "armv7l", nil }, Platform{"linux", "armv7"}, false},
		{"linux arm machine armv6l", "linux", "arm", func() (string, error) { return "armv6l", nil }, Platform{"linux", "armv6"}, false},
		{"linux arm machine bare armv7", "linux", "arm", func() (string, error) { return "armv7", nil }, Platform{"linux", "armv7"}, false},
		{"linux arm uname failure", "linux", "arm", func() (string, error) { return "", errors.New("boom") }, Platform{}, true},
		{"linux arm unknown machine", "linux", "arm", func() (string, error) { return "aarch64", nil }, Platform{}, true},
		{"unsupported os", "windows", "amd64", okMachine, Platform{}, true},
		{"unsupported os freebsd", "freebsd", "amd64", okMachine, Platform{}, true},
		{"unsupported arch", "linux", "386", okMachine, Platform{}, true},
		{"unsupported arch riscv64", "linux", "riscv64", okMachine, Platform{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := detectPlatform(tc.goos, tc.goarch, tc.machine)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("detectPlatform(%s/%s) expected error, got %+v", tc.goos, tc.goarch, got)
				}
				if !phelixerr.IsCode(err, phelixerr.CodeConfiguration) {
					t.Fatalf("error = %v, want CodeConfiguration", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("detectPlatform(%s/%s) unexpected error: %v", tc.goos, tc.goarch, err)
			}
			if got != tc.want {
				t.Fatalf("detectPlatform(%s/%s) = %+v, want %+v", tc.goos, tc.goarch, got, tc.want)
			}
		})
	}
}

// TestDetectPlatformOnHost verifies the real detection agrees with the test
// host whenever the host is a platform Phelix releases for.
func TestDetectPlatformOnHost(t *testing.T) {
	plat, err := DetectPlatform()
	supported := (runtime.GOOS == "linux" || runtime.GOOS == "darwin") &&
		(runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")
	if !supported {
		if err == nil {
			t.Fatalf("DetectPlatform() on unsupported host %s/%s should fail", runtime.GOOS, runtime.GOARCH)
		}
		return
	}
	if err != nil {
		t.Fatalf("DetectPlatform() unexpected error: %v", err)
	}
	if plat.OS != runtime.GOOS || plat.Arch != runtime.GOARCH {
		t.Fatalf("DetectPlatform() = %+v, want %s/%s", plat, runtime.GOOS, runtime.GOARCH)
	}
}
