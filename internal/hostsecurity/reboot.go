package hostsecurity

import (
	"context"
	"sort"
	"strings"
)

// RebootState reports the host's restart requirement together with the
// facts that established it. A missing fact means it could not be
// established, never that it is absent.
type RebootState struct {
	Required      bool     `json:"required"`
	Reasons       []string `json:"reasons,omitempty"`
	RunningKernel string   `json:"running_kernel,omitempty"`
	LatestKernel  string   `json:"latest_kernel,omitempty"`
}

const (
	rebootMarkerPath     = "var/run/reboot-required"
	rebootMarkerPkgsPath = "var/run/reboot-required.pkgs"
)

// DetectReboot evaluates the restart requirement. Debian and Ubuntu expose
// the restart marker and its package list; every supported manager except
// apk additionally compares the running kernel release against the newest
// installed kernel package. Alpine has no marker or kernel package
// convention, so apk reports "not required" with an explicit limitation
// instead of inventing a clean state.
func DetectReboot(ctx context.Context, system System, host Host) (RebootState, bool) {
	switch host.Manager {
	case "apt", "dnf", "zypper", "pacman":
	case "apk":
		return RebootState{Reasons: []string{"apk does not expose a restart marker or a kernel package; the reboot state cannot be established on Alpine"}}, true
	default:
		return RebootState{}, false
	}
	state := RebootState{Reasons: []string{}}
	if host.Manager == "apt" {
		if raw, err := system.read(rebootMarkerPath); err == nil && strings.TrimSpace(string(raw)) != "" {
			state.Required = true
			state.Reasons = append(state.Reasons, "the restart marker /var/run/reboot-required is present")
		}
		if raw, err := system.read(rebootMarkerPkgsPath); err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				pkg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
				if pkg != "" {
					state.Reasons = append(state.Reasons, "package "+pkg+" requested a restart")
				}
			}
		}
	}
	running := host.Kernel
	if raw, err := system.Runner.Run(ctx, "uname", "-r"); err == nil {
		if value := strings.TrimSpace(string(raw)); value != "" {
			running = value
		}
	}
	state.RunningKernel = running
	latest, stale, known := kernelState(ctx, system, host.Manager, running)
	if !known {
		return state, true
	}
	state.LatestKernel = latest
	if stale {
		state.Required = true
		state.Reasons = append(state.Reasons, "the running kernel "+running+" is older than the installed kernel "+latest)
	}
	return state, true
}

// kernelState resolves the newest installed kernel release and whether the
// running kernel is older than it. The returned flag reports whether the
// manager exposed enough information for the comparison at all.
func kernelState(ctx context.Context, system System, manager, running string) (string, bool, bool) {
	switch manager {
	case "apt":
		raw, err := system.Runner.Run(ctx, "dpkg-query", "-W", "-f=${Package}\t${db:Status-Abbrev}\t${Version}\n", "linux-image*")
		if err != nil {
			return "", false, false
		}
		installed := []string{}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
			if len(fields) < 3 || !strings.HasPrefix(fields[1], "i") {
				continue
			}
			release := strings.TrimPrefix(fields[0], "linux-image-")
			if release == fields[0] || !kernelReleaseLike(release) {
				continue
			}
			installed = append(installed, release)
		}
		return kernelReleaseState(installed, running, debianCompare)
	case "dnf", "zypper":
		raw, err := system.Runner.Run(ctx, "rpm", "-q", "kernel")
		if err != nil {
			return "", false, false
		}
		installed := []string{}
		for _, line := range strings.Split(string(raw), "\n") {
			entry := strings.TrimSpace(strings.TrimRight(line, "\r"))
			if strings.HasPrefix(entry, "kernel-") {
				installed = append(installed, strings.TrimPrefix(entry, "kernel-"))
			}
		}
		return kernelReleaseState(installed, running, rpmCompare)
	case "pacman":
		raw, err := system.Runner.Run(ctx, "pacman", "-Q", "linux")
		if err != nil {
			return "", false, false
		}
		fields := strings.Fields(string(raw))
		if len(fields) != 2 {
			return "", false, false
		}
		latest := fields[1]
		if running == "" {
			return latest, false, true
		}
		result, err := CompareVersions(ctx, system.Runner, "pacman",
			strings.ReplaceAll(running, ".", "-"), strings.ReplaceAll(latest, ".", "-"))
		if err != nil {
			return latest, false, true
		}
		return latest, result < 0, true
	}
	return "", false, false
}

// kernelReleaseState picks the newest installed kernel release and reports
// whether the running release is older than it.
func kernelReleaseState(installed []string, running string, compare func(a, b string) int) (string, bool, bool) {
	if len(installed) == 0 {
		return "", false, false
	}
	sort.Strings(installed)
	newest := installed[0]
	for _, release := range installed[1:] {
		if compare(release, newest) > 0 {
			newest = release
		}
	}
	if running == "" {
		return newest, false, true
	}
	return newest, compare(running, newest) < 0, true
}

// kernelReleaseLike reports whether a dpkg package name suffix looks like a
// versioned kernel release rather than a metapackage or debug tag.
func kernelReleaseLike(release string) bool {
	return release != "" && release[0] >= '0' && release[0] <= '9'
}
