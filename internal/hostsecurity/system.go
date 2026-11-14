package hostsecurity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type NativeRunner struct{}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("package output exceeds size limit")
	}
	return b.Buffer.Write(p)
}

func nativePath(name string) (string, error) {
	for _, dir := range []string{"/usr/bin", "/usr/sbin", "/bin", "/sbin"} {
		path := filepath.Join(dir, name)
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("required utility %s is unavailable", name)
}

func (NativeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	path, err := nativePath(name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"LC_ALL=C", "LANG=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	out := &boundedOutput{limit: 32 << 20}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s failed; check utility availability, permissions and package database health", name)
	}
	return out.Bytes(), nil
}

type System struct {
	Root   string
	Runner Runner
	Lookup func(string) (string, error)
}

func LocalSystem() System { return System{Root: "/", Runner: NativeRunner{}, Lookup: nativePath} }

func (s System) read(path string) ([]byte, error) {
	f, err := os.Open(filepath.Join(s.Root, path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return nil, err
	}
	if len(b) == 4<<20 {
		return nil, errors.New("system metadata exceeds size limit")
	}
	return b, nil
}

func ParseOSRelease(raw []byte) (map[string]string, error) {
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, errors.New("invalid os-release entry")
		}
		if strings.HasPrefix(value, "\"") {
			var err error
			value, err = strconv.Unquote(value)
			if err != nil {
				return nil, errors.New("invalid os-release quoting")
			}
		} else if strings.HasPrefix(value, "'") {
			if len(value) < 2 || value[len(value)-1] != '\'' {
				return nil, errors.New("invalid os-release quoting")
			}
			value = value[1 : len(value)-1]
		}
		if _, ok := values[key]; ok {
			return nil, errors.New("duplicate os-release field")
		}
		values[key] = value
	}
	if values["ID"] == "" {
		return nil, errors.New("os-release is missing ID")
	}
	return values, nil
}

func (s System) Detect(ctx context.Context) (Host, error) {
	if err := ctx.Err(); err != nil {
		return Host{Status: "unavailable"}, err
	}
	h := Host{Status: "unavailable", Architecture: runtime.GOARCH}
	raw, err := s.read("etc/os-release")
	if os.IsNotExist(err) {
		raw, err = s.read("usr/lib/os-release")
	}
	if err != nil {
		return h, fmt.Errorf("cannot read os-release: %w", err)
	}
	v, err := ParseOSRelease(raw)
	if err != nil {
		return h, err
	}
	h.ID = v["ID"]
	h.Name = v["PRETTY_NAME"]
	if h.Name == "" {
		h.Name = v["NAME"]
	}
	h.Version = v["VERSION_ID"]
	h.Codename = v["VERSION_CODENAME"]
	switch h.ID {
	case "ubuntu", "debian":
		h.Manager = "apt"
	case "fedora", "rhel", "rocky", "almalinux":
		h.Manager = "dnf"
	case "arch":
		h.Manager = "pacman"
		if h.Version == "" {
			h.Version = "rolling"
		}
	case "alpine":
		h.Manager = "apk"
	case "opensuse", "opensuse-leap", "opensuse-tumbleweed":
		h.Manager = "zypper"
	default:
		h.Status = "unsupported"
		h.Reason = "distribution has no supported package inventory adapter"
		return h, nil
	}
	if h.Version == "" {
		h.Reason = "distribution version is missing"
		return h, nil
	}
	lookup := s.Lookup
	if lookup == nil {
		lookup = nativePath
	}
	utilities := []string{h.Manager}
	if h.Manager == "apt" {
		utilities = append(utilities, "dpkg-query")
	}
	if h.Manager == "dnf" || h.Manager == "zypper" {
		utilities = append(utilities, "rpm")
	}
	for _, name := range utilities {
		if _, err := lookup(name); err != nil {
			h.Reason = err.Error()
			return h, nil
		}
	}
	raw, err = s.Runner.Run(ctx, h.Manager, "--version")
	if err != nil {
		h.Reason = err.Error()
		if ctx.Err() != nil {
			return h, ctx.Err()
		}
		return h, nil
	}
	h.ManagerVersion = strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
	if len(h.ManagerVersion) > 200 {
		h.ManagerVersion = h.ManagerVersion[:200]
	}
	if raw, err = s.read("proc/sys/kernel/osrelease"); err == nil {
		h.Kernel = strings.TrimSpace(string(raw))
	}
	if raw, err = s.Runner.Run(ctx, "uname", "-m"); err == nil {
		h.Architecture = strings.TrimSpace(string(raw))
	} else if ctx.Err() != nil {
		return h, ctx.Err()
	}
	if raw, err = s.read("etc/hostname"); err == nil {
		h.Hostname = strings.TrimSpace(string(raw))
	}
	if raw, err = s.read("etc/machine-id"); err == nil && len(bytes.TrimSpace(raw)) > 0 {
		sum := sha256.Sum256(bytes.TrimSpace(raw))
		h.MachineHash = hex.EncodeToString(sum[:])
	}
	h.Status = "supported"
	return h, nil
}
