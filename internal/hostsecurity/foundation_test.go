package hostsecurity

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRunner func(context.Context, string, ...string) ([]byte, error)

func (f fakeRunner) Run(c context.Context, n string, a ...string) ([]byte, error) {
	return f(c, n, a...)
}

func fixtureSystem(t *testing.T, id string) System {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID="+id+"\nVERSION_ID=12\nPRETTY_NAME=\"Fixture Linux\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return System{Root: root, Lookup: func(n string) (string, error) { return n, nil }, Runner: fakeRunner(func(context.Context, string, ...string) ([]byte, error) { return []byte("1.0\n"), nil })}
}

func TestDetection(t *testing.T) {
	for id, manager := range map[string]string{"ubuntu": "apt", "debian": "apt", "fedora": "dnf", "rhel": "dnf", "rocky": "dnf", "almalinux": "dnf", "arch": "pacman", "alpine": "apk", "opensuse": "zypper", "opensuse-leap": "zypper", "opensuse-tumbleweed": "zypper"} {
		t.Run(id, func(t *testing.T) {
			h, err := fixtureSystem(t, id).Detect(context.Background())
			if err != nil || h.Status != "supported" || h.Manager != manager {
				t.Fatalf("%+v %v", h, err)
			}
		})
	}
	s := fixtureSystem(t, "custom")
	h, err := s.Detect(context.Background())
	if err != nil || h.Status != "unsupported" {
		t.Fatalf("%+v %v", h, err)
	}
	s = fixtureSystem(t, "debian")
	s.Lookup = func(string) (string, error) { return "", errors.New("missing utility") }
	h, err = s.Detect(context.Background())
	if err != nil || h.Status != "unavailable" {
		t.Fatalf("%+v %v", h, err)
	}
	for _, raw := range []string{"NAME=test", "ID=\"bad", "ID=debian\nID=ubuntu", "ID"} {
		if _, err := ParseOSRelease([]byte(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestInventoryFormats(t *testing.T) {
	dpkg, err := ParseDPKG([]byte("hi \tlibssl3\t1:3.0.11-1~deb12u2\tamd64\topenssl\t1:3.0.11-1~deb12u2\tDebian\tssl-api\tlibc6 (>= 2.34)\nrc \tremoved\t1\tall\tremoved\t1\t\t\t\n"))
	if err != nil || len(dpkg) != 1 || dpkg[0].Held != "yes" || dpkg[0].Source != "openssl" {
		t.Fatalf("%+v %v", dpkg, err)
	}
	rpm, err := ParseRPM([]byte("openssl-libs\t1\t3.0.7\t27.el9\tx86_64\topenssl-3.0.7-27.el9.src.rpm\tRed Hat\n"))
	if err != nil || len(rpm) != 1 || rpm[0].Version != "1:3.0.7-27.el9" || rpm[0].Source != "openssl" {
		t.Fatalf("%+v %v", rpm, err)
	}
	apk, err := ParseAPK([]byte("P:libssl3\nV:3.1.4-r2\nA:x86_64\no:openssl\nD:so:libc.musl-x86_64.so.1\np:so:libssl.so.3=3\n\n"))
	if err != nil || len(apk) != 1 || apk[0].Source != "openssl" {
		t.Fatalf("%+v %v", apk, err)
	}
	pacman, err := ParsePacman([]byte("%NAME%\nopenssl\n\n%VERSION%\n3.2.1-1\n\n%ARCH%\nx86_64\n\n%DEPENDS%\nglibc\n\n"))
	if err != nil || pacman.Name != "openssl" || len(pacman.Depends) != 1 {
		t.Fatalf("%+v %v", pacman, err)
	}
	for _, parse := range []func([]byte) ([]Package, error){ParseDPKG, ParseRPM, ParseAPK} {
		if _, err := parse([]byte("broken")); err == nil {
			t.Fatal("accepted malformed package record")
		}
	}
}

func TestDistributionVersions(t *testing.T) {
	for _, tc := range []struct {
		manager, a, b string
		want          int
	}{
		{"apt", "1:1.0-1", "2.0-1", 1}, {"apt", "1.0~rc1-1", "1.0-1", -1}, {"apt", "1.0-1+deb12u2", "1.0-1+deb12u10", -1}, {"apt", "1.0", "1.0-0", 0}, {"apt", "1.0-01", "1.0-1", 0}, {"apt", "1.0a", "1.0+", -1},
		{"dnf", "1:1.0-1.el9", "0:2.0-1.el9", 1}, {"dnf", "1.0~rc1-1", "1.0-1", -1}, {"dnf", "1.0^git1-1", "1.0-1", 1}, {"dnf", "1.0^git1-1", "1.0.1-1", -1}, {"zypper", "1.0-150600.10.2", "1.0-150600.10.10", -1}, {"dnf", "1.0-1", "1.0-1", 0},
	} {
		got, err := CompareVersions(context.Background(), nil, tc.manager, tc.a, tc.b)
		if err != nil || got != tc.want {
			t.Fatalf("%+v: %d %v", tc, got, err)
		}
		reverse, _ := CompareVersions(context.Background(), nil, tc.manager, tc.b, tc.a)
		if reverse != -got {
			t.Fatal("comparison not antisymmetric")
		}
	}
	for manager, output := range map[string]string{"apk": "<", "pacman": "-1"} {
		r := fakeRunner(func(ctx context.Context, n string, a ...string) ([]byte, error) {
			if n != "apk" && n != "vercmp" {
				t.Fatal(n)
			}
			return []byte(output), nil
		})
		if got, err := CompareVersions(context.Background(), r, manager, "1.0-r0", "1.0-r1"); err != nil || got != -1 {
			t.Fatalf("%d %v", got, err)
		}
	}
	if _, err := CompareVersions(context.Background(), nil, "apt", "1;id", "2"); err == nil {
		t.Fatal("accepted invalid version")
	}
	if _, err := exec.LookPath("vercmp"); err == nil {
		if got, err := CompareVersions(context.Background(), NativeRunner{}, "pacman", "1:1.0-2", "1:1.0-10"); err != nil || got != -1 {
			t.Fatalf("native vercmp: %d %v", got, err)
		}
	}
}

func TestPersistenceAndCancellation(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := fixtureSystem(t, "debian")
	s.Runner = fakeRunner(func(ctx context.Context, n string, a ...string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if n == "dpkg-query" {
			return []byte("ii \tssl\t1.0-1\tamd64\tssl\t1.0-1\t\t\t\n"), nil
		}
		return []byte("1.0"), nil
	})
	scan, err := Collect(context.Background(), s, store, "test-server", nil)
	if err != nil || scan.PackageCount != 1 || scan.Coverage != "unknown" {
		t.Fatalf("%+v %v", scan, err)
	}
	page, err := store.Inventory(context.Background(), "test-server", "", "ssl", 1, 0)
	if err != nil || page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	mixed, err := store.Inventory(context.Background(), "test-server", "", "SSL", 1, 0)
	if err != nil || mixed.Total != 1 || len(mixed.Items) != 1 {
		t.Fatalf("inventory search must be case-insensitive: %+v %v", mixed, err)
	}
	absent, err := store.Inventory(context.Background(), "test-server", "", "openssl", 1, 0)
	if err != nil || absent.Total != 0 || len(absent.Items) != 0 {
		t.Fatalf("inventory search must stay a substring match: %+v %v", absent, err)
	}
	if _, err = store.Scan(context.Background(), "other-server", scan.ID); err == nil {
		t.Fatal("cross-server scan read")
	}
	if err = store.SaveScan(context.Background(), scan, page.Items); err == nil {
		t.Fatal("overwrote immutable scan")
	}
	if _, err = store.DB.Exec("UPDATE host_scans SET status='safe'"); err == nil {
		t.Fatal("mutable historical record")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled, err := Collect(ctx, s, store, "test-server", nil)
	if !errors.Is(err, context.Canceled) || cancelled.Status != "cancelled" {
		t.Fatalf("%+v %v", cancelled, err)
	}
	history, err := store.History(context.Background(), "test-server", 10, 0)
	if err != nil || history.Total != 2 {
		t.Fatalf("%+v %v", history, err)
	}
	if _, err := store.Inventory(context.Background(), "test-server", scan.ID, "", 501, 0); err == nil {
		t.Fatal("unbounded page")
	}
}

func TestSnapshotStableAndMissingDatabase(t *testing.T) {
	s := fixtureSystem(t, "pacman")
	h := Host{Status: "supported", Manager: "pacman"}
	if _, _, err := s.Inventory(context.Background(), h, time.Now()); err == nil {
		t.Fatal("missing db accepted")
	}
	path := filepath.Join(s.Root, "var/lib/pacman/local/openssl-1")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "desc"), []byte("%NAME%\nopenssl\n%VERSION%\n1.0-1\n%ARCH%\nx86_64\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, a, err := s.Inventory(context.Background(), h, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, b, err := s.Inventory(context.Background(), h, time.Now().Add(time.Hour))
	if err != nil || a != b {
		t.Fatalf("snapshot depends on clock: %v", err)
	}
	buffer := &boundedOutput{limit: 2}
	if _, err := buffer.Write([]byte(strings.Repeat("a", 3))); err == nil {
		t.Fatal("output bound ignored")
	}
}
