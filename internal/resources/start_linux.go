//go:build linux

package resources

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const cleanupArg = "--phelix-internal-cgroup-cleanup"

// group owns only a freshly created leaf. All operations remain fd-relative.
type group struct {
	parent *os.File
	dir    *os.File
	name   string
}

func (g *group) close() { _ = g.dir.Close(); _ = g.parent.Close() }

func writeControl(dir *os.File, name, value string) error {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	_, err = io.WriteString(f, value)
	return errors.Join(err, f.Close())
}

func readControl(dir *os.File, name string) ([]byte, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 65536))
}

func (g *group) remove() error {
	err := unix.Unlinkat(int(g.parent.Fd()), g.name, unix.AT_REMOVEDIR)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

func cgroupFS(f *os.File) error {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &stat); err != nil {
		return err
	}
	if stat.Type != unix.CGROUP2_SUPER_MAGIC {
		return fmt.Errorf("not a cgroups v2 filesystem")
	}
	return nil
}

func openDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func mountPath(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// resolveRoot accounts for bind-mounted subtrees and cgroup namespaces.
func resolveRoot(mountinfo, membership, override string) (string, error) {
	current := ""
	for _, line := range strings.Split(membership, "\n") {
		if strings.HasPrefix(line, "0::") {
			current = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if override != "" && (!filepath.IsAbs(override) || filepath.Clean(override) != override) {
		return "", fmt.Errorf("PHELIX_CGROUP_ROOT must be a clean absolute delegated cgroup path")
	}
	for _, line := range strings.Split(mountinfo, "\n") {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		before, after := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(before) < 5 || len(after) < 1 || after[0] != "cgroup2" {
			continue
		}
		root, mount := mountPath(before[3]), mountPath(before[4])
		if override != "" {
			if within(mount, override) {
				return override, nil
			}
			continue
		}
		if current == "" || !filepath.IsAbs(current) || filepath.Clean(current) != current {
			continue
		}
		if within(root, current) {
			rel, _ := filepath.Rel(root, current)
			return filepath.Join(mount, rel), nil
		}
	}
	return "", fmt.Errorf("resource limits require Linux cgroups v2, but no accessible cgroup2 mount matches the current cgroup or PHELIX_CGROUP_ROOT")
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func discoverRoot() (string, error) {
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	membership, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	return resolveRoot(string(mounts), string(membership), os.Getenv("PHELIX_CGROUP_ROOT"))
}

func createGroup(l Limits) (_ *group, errRet error) {
	root, err := discoverRoot()
	if err != nil {
		return nil, fmt.Errorf("resource limits: discover cgroups v2: %w", err)
	}
	parent, err := openDirectory(root)
	if err != nil {
		return nil, fmt.Errorf("resource limits: open delegated cgroup %q: %w", root, err)
	}
	defer func() {
		if errRet != nil {
			_ = parent.Close()
		}
	}()
	if err := cgroupFS(parent); err != nil {
		return nil, fmt.Errorf("resource limits: %s: %w", root, err)
	}
	controllers, err := readControl(parent, "cgroup.controllers")
	if err != nil {
		return nil, fmt.Errorf("resource limits: read controllers: %w", err)
	}
	enabled, err := readControl(parent, "cgroup.subtree_control")
	if err != nil {
		return nil, fmt.Errorf("resource limits: read delegated controllers: %w", err)
	}
	for controller, needed := range map[string]bool{"cpu": l.CPUQuota != 0, "memory": l.MemoryBytes != 0} {
		if needed && (!slices.Contains(strings.Fields(string(controllers)), controller) || !slices.Contains(strings.Fields(string(enabled)), controller)) {
			return nil, fmt.Errorf("resource limits: %s controller is not enabled for children of %s; configure an empty delegated parent with %s in cgroup.subtree_control and set PHELIX_CGROUP_ROOT", controller, root, controller)
		}
	}
	return createLeaf(parent, l)
}

func createLeaf(parent *os.File, l Limits) (_ *group, errRet error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("phelix-%x", token)
	if err := unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
		return nil, fmt.Errorf("resource limits: create instance cgroup under %s (delegation required): %w", parent.Name(), err)
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.Join(err, unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR))
	}
	g := &group{parent: parent, dir: os.NewFile(uintptr(fd), name), name: name}
	defer func() {
		if errRet != nil {
			errRet = errors.Join(errRet, g.remove())
			_ = g.dir.Close()
		}
	}()
	if err := configureGroup(g.dir, l); err != nil {
		return nil, fmt.Errorf("resource limits: configure instance cgroup: %w", err)
	}
	return g, nil
}

func configureGroup(dir *os.File, l Limits) error {
	if l.CPUQuota != 0 {
		if err := writeControl(dir, "cpu.max", fmt.Sprintf("%d %d", l.CPUQuota, CPUPeriod)); err != nil {
			return fmt.Errorf("cpu.max: %w", err)
		}
	}
	if l.MemoryBytes != 0 {
		if err := writeControl(dir, "memory.max", fmt.Sprint(l.MemoryBytes)); err != nil {
			return fmt.Errorf("memory.max: %w", err)
		}
	}
	if _, err := readControl(dir, "cgroup.procs"); err != nil {
		return fmt.Errorf("cgroup.procs: %w", err)
	}
	if _, err := readControl(dir, "cgroup.events"); err != nil {
		return fmt.Errorf("cgroup.events: %w", err)
	}
	return nil
}

// Start uses CLONE_INTO_CGROUP: the child belongs to the limited cgroup before
// it can execute or fork. Unsupported kernels/seccomp policies fail closed.
func Start(cmd *exec.Cmd, cfg Config) error {
	l, err := cfg.Limits()
	if err != nil {
		return err
	}
	if cfg.IsZero() {
		return cmd.Start()
	}
	g, err := createGroup(l)
	if err != nil {
		return err
	}
	defer g.close()
	lease, err := startCleanup(g, cmd.Stderr)
	if err != nil {
		return errors.Join(fmt.Errorf("resource limits: start cleanup helper: %w", err), g.remove())
	}
	defer lease.Close()
	return startInGroup(cmd, g, (*exec.Cmd).Start)
}

func startInGroup(cmd *exec.Cmd, g *group, spawn func(*exec.Cmd) error) error {
	original := cmd.SysProcAttr
	attr := syscall.SysProcAttr{}
	if original != nil {
		attr = *original
	}
	attr.UseCgroupFD, attr.CgroupFD = true, int(g.dir.Fd())
	cmd.SysProcAttr = &attr
	err := spawn(cmd)
	cmd.SysProcAttr = original
	if err != nil {
		return errors.Join(fmt.Errorf("resource limits: atomic cgroup launch failed (Linux 5.7+ and clone3 permission required): %w", err), g.remove())
	}
	return nil
}

func startCleanup(g *group, stderr io.Writer) (*os.File, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	leaseR, leaseW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer leaseR.Close()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		leaseW.Close()
		return nil, err
	}
	defer readyR.Close()
	defer readyW.Close()
	helper := exec.Command(exe, cleanupArg, g.name)
	helper.ExtraFiles = []*os.File{g.parent, g.dir, leaseR, readyW}
	helper.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Do not keep a caller's capture pipe or terminal alive after CLI exit.
	if file, ok := stderr.(*os.File); ok {
		helper.Stderr = file
	}
	if err := helper.Start(); err != nil {
		leaseW.Close()
		return nil, err
	}
	_ = readyW.Close()
	done := make(chan error, 1)
	go func() { done <- helper.Wait() }()
	ready := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := io.ReadFull(readyR, b[:])
		if err == nil && b[0] != 1 {
			err = fmt.Errorf("invalid cleanup readiness response")
		}
		ready <- err
	}()
	select {
	case err = <-ready:
	case <-time.After(5 * time.Second):
		err = fmt.Errorf("cleanup helper readiness timed out")
	}
	if err != nil {
		_ = leaseW.Close()
		_ = helper.Process.Kill()
		<-done
		return nil, err
	}
	return leaseW, nil
}

// RunCleanupHelper is an internal reexec entrypoint, not a user CLI command.
// The inherited lease prevents removal while the launcher is attaching a PID.
func RunCleanupHelper() bool {
	if len(os.Args) != 3 || os.Args[1] != cleanupArg {
		return false
	}
	parent, dir := os.NewFile(3, "cgroup-parent"), os.NewFile(4, "cgroup-instance")
	lease, ready := os.NewFile(5, "launch-lease"), os.NewFile(6, "cleanup-ready")
	defer parent.Close()
	defer dir.Close()
	defer lease.Close()
	defer ready.Close()
	name := os.Args[2]
	if len(name) != len("phelix-")+32 || !strings.HasPrefix(name, "phelix-") || filepath.Base(name) != name {
		return true
	}
	if cgroupFS(parent) != nil || cgroupFS(dir) != nil {
		return true
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return true
	}
	check := os.NewFile(uintptr(fd), name)
	a, aerr := check.Stat()
	b, berr := dir.Stat()
	check.Close()
	if aerr != nil || berr != nil || !os.SameFile(a, b) {
		return true
	}
	if _, err := readControl(dir, "cgroup.events"); err != nil {
		return true
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		return true
	}
	ready.Close()
	_, _ = io.Copy(io.Discard, lease)
	g := &group{parent: parent, dir: dir, name: name}
	for {
		data, err := readControl(dir, "cgroup.events")
		if errors.Is(err, unix.ENOENT) {
			return true
		}
		if err != nil {
			log.Printf("resource limits: cleanup %s: %v", name, err)
			return true
		}
		empty, err := unpopulated(data)
		if err != nil {
			log.Printf("resource limits: cleanup %s: %v", name, err)
			return true
		}
		if empty {
			err = g.remove()
			if err == nil {
				return true
			}
			if !errors.Is(err, unix.EBUSY) {
				log.Printf("resource limits: remove cgroup %s: %v", name, err)
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func unpopulated(data []byte) (bool, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "populated" {
			if fields[1] == "0" {
				return true, nil
			}
			if fields[1] == "1" {
				return false, nil
			}
		}
	}
	return false, fmt.Errorf("cgroup.events has no valid populated field")
}
