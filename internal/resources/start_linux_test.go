//go:build linux

package resources

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func testGroup(t *testing.T) *group {
	t.Helper()
	root := t.TempDir()
	parent, err := openDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	name := "phelix-0123456789abcdef0123456789abcdef"
	path := filepath.Join(root, name)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := openDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	g := &group{parent: parent, dir: dir, name: name}
	t.Cleanup(g.close)
	return g
}

func TestResolveRoot(t *testing.T) {
	mounts := "25 20 0:24 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n"
	for _, tc := range []struct {
		name, mounts, membership, override, want string
	}{
		{"unified", mounts, "0::/user.slice/session.scope\n", "", "/sys/fs/cgroup/user.slice/session.scope"},
		{"override", mounts, "0::/launcher\n", "/sys/fs/cgroup/delegated", "/sys/fs/cgroup/delegated"},
		{"bind mount", "25 20 0:24 /delegated /cg rw - cgroup2 cgroup rw", "0::/delegated/launcher", "", "/cg/launcher"},
		{"namespace", mounts, "0::/", "", "/sys/fs/cgroup"},
		{"escaped mount", `25 20 0:24 / /my\040cgroup rw - cgroup2 cgroup rw`, "0::/app", "", "/my cgroup/app"},
		{"v1", "25 20 0:24 / /sys/fs/cgroup rw - cgroup cgroup rw", "1:cpu:/app", "", ""},
		{"missing membership", mounts, "", "", ""},
		{"relative override", mounts, "0::/", "delegated", ""},
		{"unclean override", mounts, "0::/", "/sys/fs/cgroup/../other", ""},
		{"outside mount", mounts, "0::/", "/sys/fs/cgroup-other", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveRoot(tc.mounts, tc.membership, tc.override)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("accepted unsupported root %q", got)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestConfigureGroup(t *testing.T) {
	for _, tc := range []struct {
		name        string
		limits      Limits
		cpu, memory string
	}{
		{"both", Limits{CPUQuota: 50000, MemoryBytes: 536870912}, "50000 100000", "536870912"},
		{"cpu only", Limits{CPUQuota: 200000}, "200000 100000", ""},
		{"memory only", Limits{MemoryBytes: 1073741824}, "", "1073741824"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := testGroup(t)
			for _, name := range []string{"cpu.max", "memory.max", "cgroup.procs", "cgroup.events"} {
				if err := os.WriteFile(filepath.Join(g.dir.Name(), name), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := configureGroup(g.dir, tc.limits); err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{"cpu.max": tc.cpu, "memory.max": tc.memory} {
				got, err := readControl(g.dir, name)
				if err != nil || string(got) != want {
					t.Fatalf("%s = %q, %v; want %q", name, got, err, want)
				}
			}
		})
	}
}

func TestConfigureGroupPartialFailure(t *testing.T) {
	g := testGroup(t)
	if err := os.WriteFile(filepath.Join(g.dir.Name(), "cpu.max"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	err := configureGroup(g.dir, Limits{CPUQuota: 50000, MemoryBytes: 536870912})
	if err == nil || !strings.Contains(err.Error(), "memory.max") {
		t.Fatalf("expected memory configuration failure, got %v", err)
	}
	cpu, err := readControl(g.dir, "cpu.max")
	if err != nil || string(cpu) != "50000 100000" {
		t.Fatalf("CPU configuration not exercised: %q, %v", cpu, err)
	}
}

func TestCreateLeafFailureCleansUp(t *testing.T) {
	root := t.TempDir()
	parent, err := openDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	// Ordinary directories do not provide kernel-created control files.
	g, err := createLeaf(parent, Limits{CPUQuota: 50000})
	if err == nil || g != nil {
		t.Fatalf("expected failed configuration: %v, %v", g, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial cgroup leaked: %v, %v", entries, err)
	}
}

func TestAtomicLaunch(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			g := testGroup(t)
			original := &syscall.SysProcAttr{Setsid: true}
			cmd := exec.Command("unused-test-command")
			cmd.SysProcAttr = original
			spawnError := errors.New("clone3 denied")
			called := false
			err := startInGroup(cmd, g, func(c *exec.Cmd) error {
				called = true
				if !c.SysProcAttr.UseCgroupFD || c.SysProcAttr.CgroupFD != int(g.dir.Fd()) || !c.SysProcAttr.Setsid {
					t.Fatalf("missing atomic attachment or lost process attributes: %+v", c.SysProcAttr)
				}
				if fail {
					return spawnError
				}
				return nil
			})
			if !called || cmd.SysProcAttr != original || original.UseCgroupFD {
				t.Fatal("spawn not called or original attributes mutated")
			}
			_, statErr := os.Stat(g.dir.Name())
			if fail {
				if !errors.Is(err, spawnError) || !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("launch failure lost cause or leaked cgroup: %v, %v", err, statErr)
				}
			} else if err != nil || statErr != nil {
				t.Fatalf("successful launch removed cgroup: %v, %v", err, statErr)
			}
		})
	}
}

func TestRemoveIdempotent(t *testing.T) {
	g := testGroup(t)
	for i := 0; i < 2; i++ {
		if err := g.remove(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnpopulated(t *testing.T) {
	for _, tc := range []struct {
		data           string
		empty, invalid bool
	}{
		{"populated 0\nfrozen 0\n", true, false},
		{"populated 1\n", false, false},
		{"populated 2\n", false, true},
		{"frozen 0\n", false, true},
	} {
		empty, err := unpopulated([]byte(tc.data))
		if empty != tc.empty || (err != nil) != tc.invalid {
			t.Fatalf("%q = %v, %v", tc.data, empty, err)
		}
	}
}

func TestUnlimitedStartBypassesCgroups(t *testing.T) {
	t.Setenv("PHELIX_CGROUP_ROOT", "invalid-relative-root")
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := Start(cmd, Config{}); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
