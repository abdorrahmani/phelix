package hostsecurity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRebootMarker(t *testing.T, root string, packages string) {
	t.Helper()
	dir := filepath.Join(root, "var", "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reboot-required"), []byte("*** System restart required ***\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reboot-required.pkgs"), []byte(packages), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDetectRebootMarkerAndPackages(t *testing.T) {
	system := fixtureSystem(t, "debian")
	writeRebootMarker(t, system.Root, "- libssl3\nopenssl\n")
	state, known := DetectReboot(context.Background(), system, remediationHost("apt"))
	if !known || !state.Required {
		t.Fatalf("state: %+v known %v", state, known)
	}
	if state.Reasons[0] != "the restart marker /var/run/reboot-required is present" {
		t.Fatalf("marker reason: %+v", state.Reasons)
	}
	if len(state.Reasons) != 3 || state.Reasons[1] != "package libssl3 requested a restart" || state.Reasons[2] != "package openssl requested a restart" {
		t.Fatalf("package reasons: %+v", state.Reasons)
	}
}

func TestDetectRebootKernelComparison(t *testing.T) {
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "uname" && len(args) == 1 && args[0] == "-r" {
			return []byte("6.1.0-18-amd64\n"), nil
		}
		if name == "dpkg-query" && args[len(args)-1] == "linux-image*" {
			return []byte("linux-image-6.1.0-18-amd64\tii \t6.1.0-18\nlinux-image-6.1.0-20-amd64\tii \t6.1.0-20\nlinux-image-amd64\tii \t6.1.0.20\n"), nil
		}
		return []byte(""), nil
	})
	state, known := DetectReboot(context.Background(), system, remediationHost("apt"))
	if !known || !state.Required {
		t.Fatalf("stale kernel state: %+v", state)
	}
	if state.RunningKernel != "6.1.0-18-amd64" || state.LatestKernel != "6.1.0-20-amd64" {
		t.Fatalf("kernels: %+v", state)
	}
	if len(state.Reasons) != 1 || !strings.Contains(state.Reasons[0], "6.1.0-18-amd64 is older than the installed kernel 6.1.0-20-amd64") {
		t.Fatalf("reasons: %+v", state.Reasons)
	}
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "uname" && len(args) == 1 && args[0] == "-r" {
			return []byte("6.1.0-20-amd64\n"), nil
		}
		if name == "dpkg-query" && args[len(args)-1] == "linux-image*" {
			return []byte("linux-image-6.1.0-20-amd64\tii \t6.1.0-20\n"), nil
		}
		return []byte(""), nil
	})
	state, known = DetectReboot(context.Background(), system, remediationHost("apt"))
	if !known || state.Required || len(state.Reasons) != 0 {
		t.Fatalf("current kernel state: %+v known %v", state, known)
	}
	if _, err := system.Runner.Run(context.Background(), "uname", "-r"); err != nil {
		t.Fatal(err)
	}
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "dpkg-query" {
			return []byte(""), errors.New("dpkg unavailable")
		}
		return []byte("6.1.0-18-amd64\n"), nil
	})
	state, known = DetectReboot(context.Background(), system, remediationHost("apt"))
	if !known || state.Required || state.LatestKernel != "" || state.RunningKernel != "6.1.0-18-amd64" {
		t.Fatalf("unavailable kernel data: %+v known %v", state, known)
	}
}

func TestDetectRebootRPMKernel(t *testing.T) {
	system := fixtureSystem(t, "fedora")
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "uname" && len(args) == 1 && args[0] == "-r" {
			return []byte("6.2.3-300.fc38.x86_64\n"), nil
		}
		if name == "rpm" && strings.Join(args, " ") == "-q kernel" {
			return []byte("kernel-6.2.4-300.fc38.x86_64\nkernel-core-6.2.4-300.fc38.x86_64\n"), nil
		}
		return []byte(""), nil
	})
	state, known := DetectReboot(context.Background(), system, remediationHost("dnf"))
	if !known || !state.Required || state.LatestKernel != "6.2.4-300.fc38.x86_64" {
		t.Fatalf("rpm stale state: %+v known %v", state, known)
	}
	if len(state.Reasons) != 1 || !strings.Contains(state.Reasons[0], "6.2.3-300.fc38.x86_64 is older") {
		t.Fatalf("rpm reasons: %+v", state.Reasons)
	}
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "rpm" {
			return []byte(""), errors.New("rpm query failed")
		}
		return []byte("6.2.3-300.fc38.x86_64\n"), nil
	})
	state, known = DetectReboot(context.Background(), system, remediationHost("dnf"))
	if !known || state.Required || state.LatestKernel != "" {
		t.Fatalf("rpm without kernel query: %+v known %v", state, known)
	}
}

func TestDetectRebootPacmanKernel(t *testing.T) {
	system := fixtureSystem(t, "arch")
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "uname" && len(args) == 1 && args[0] == "-r" {
			return []byte("6.6.1-arch1-1\n"), nil
		}
		if name == "pacman" && strings.Join(args, " ") == "-Q linux" {
			return []byte("linux 6.6.7.arch2-1\n"), nil
		}
		if name == "vercmp" {
			return []byte("-1\n"), nil
		}
		return []byte(""), nil
	})
	state, known := DetectReboot(context.Background(), system, remediationHost("pacman"))
	if !known || !state.Required || state.LatestKernel != "6.6.7.arch2-1" {
		t.Fatalf("pacman stale state: %+v known %v", state, known)
	}
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "uname" && len(args) == 1 && args[0] == "-r" {
			return []byte("6.6.7-arch2-1\n"), nil
		}
		if name == "pacman" && strings.Join(args, " ") == "-Q linux" {
			return []byte("linux 6.6.7.arch2-1\n"), nil
		}
		if name == "vercmp" {
			return []byte("0\n"), nil
		}
		return []byte(""), nil
	})
	state, known = DetectReboot(context.Background(), system, remediationHost("pacman"))
	if !known || state.Required || len(state.Reasons) != 0 {
		t.Fatalf("pacman current state: %+v known %v", state, known)
	}
}

func TestDetectRebootAPKAndUnsupportedManagers(t *testing.T) {
	system := fixtureSystem(t, "alpine")
	state, known := DetectReboot(context.Background(), system, remediationHost("apk"))
	if !known || state.Required {
		t.Fatalf("apk state: %+v known %v", state, known)
	}
	if len(state.Reasons) != 1 || !strings.Contains(state.Reasons[0], "apk does not expose a restart marker or a kernel package") {
		t.Fatalf("apk limitation: %+v", state.Reasons)
	}
	state, known = DetectReboot(context.Background(), system, remediationHost("portage"))
	if known {
		t.Fatalf("unsupported manager reported known state: %+v", state)
	}
}

func TestScanHostWiresRebootState(t *testing.T) {
	system := fixtureSystem(t, "debian")
	writeRebootMarker(t, system.Root, "- linux-image-6.1.0-18-amd64\n")
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "dpkg-query" && len(args) == 2 {
			return []byte("ii \tssl\t1.0-1\tamd64\tssl\t1.0-1\t\t\t\n"), nil
		}
		return []byte("1.0\n"), nil
	})
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scan, err := Collect(context.Background(), system, store, "test-server", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !scan.RebootRequired || len(scan.RebootReasons) != 2 || !strings.Contains(scan.RebootReasons[0], "restart marker") {
		t.Fatalf("scan reboot state: %+v", scan.RebootReasons)
	}
}
