package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/abdorrahmani/phelix/config"
	"github.com/abdorrahmani/phelix/internal/logs"
)

// ============================================================================
// Server settings
//
// ServerConnection / ServerAlert / ServerSecurity mirror the backend's
// per-server settings model. The CLI auto-detects the server's REAL current
// configuration, persists the effective settings to ~/.phelix/settings.json
// (0600, survives restarts), and reports them inside the ServerInfo snapshot
// on every MonitorStream (re)connection.
//
// The reported values are the host's actual configuration (sshd Port and
// PermitRootLogin, real listening TCP ports, firewall/auto-update state, the
// operator's real SSH keypair and login user, and the agent's own alert
// configuration), not product defaults.
// ============================================================================

// Fallbacks for auto-detection. Each detector prefers the host's real value
// and falls back to these only when the value cannot be determined.
const (
	defaultSSHPort          = 22
	defaultSSHUser          = "phelix" // last resort when $USER/whoami both fail
	defaultAuthMethod       = "key"
	defaultCPUThreshold     = 80.0
	defaultRAMThreshold     = 90.0
	defaultDiskThreshold    = 90.0
	defaultSSHRootLogin     = "prohibit-password"
	defaultSettingsFileMode = 0o600
)

// serverSettingsFileName is the settings file name under ~/.phelix.
const serverSettingsFileName = "settings.json"

// ServerConnection mirrors the backend's ServerConnection model.
type ServerConnection struct {
	SSHPort     int    `json:"ssh_port" yaml:"ssh_port"`
	SSHUser     string `json:"ssh_user" yaml:"ssh_user"`
	AuthMethod  string `json:"auth_method" yaml:"auth_method"` // "key" or "password"
	SSHPassword string `json:"ssh_password,omitempty" yaml:"ssh_password,omitempty"`
	PrivateKey  string `json:"private_key,omitempty" yaml:"private_key,omitempty"`
	PublicKey   string `json:"public_key,omitempty" yaml:"public_key,omitempty"`
}

// ServerAlert mirrors the backend's ServerAlert model.
type ServerAlert struct {
	CPUThreshold          float64 `json:"cpu_threshold" yaml:"cpu_threshold"`
	RAMThreshold          float64 `json:"ram_threshold" yaml:"ram_threshold"`
	DiskThreshold         float64 `json:"disk_threshold" yaml:"disk_threshold"`
	CPUSpikeAlerts        bool    `json:"cpu_spike_alerts" yaml:"cpu_spike_alerts"`
	MemoryPressureAlerts  bool    `json:"memory_pressure_alerts" yaml:"memory_pressure_alerts"`
	DiskSpaceAlerts       bool    `json:"disk_space_alerts" yaml:"disk_space_alerts"`
	AppCrashAlerts        bool    `json:"app_crash_alerts" yaml:"app_crash_alerts"`
	AgentDisconnectAlerts bool    `json:"agent_disconnect_alerts" yaml:"agent_disconnect_alerts"`
	WeeklyDigest          bool    `json:"weekly_digest" yaml:"weekly_digest"`
}

// ServerSecurity mirrors the backend's ServerSecurity model.
type ServerSecurity struct {
	FirewallEnabled    bool     `json:"firewall_enabled" yaml:"firewall_enabled"`
	AutoUpdates        bool     `json:"auto_updates" yaml:"auto_updates"`
	SSHRootLogin       string   `json:"ssh_root_login" yaml:"ssh_root_login"`
	IPAllowlistEnabled bool     `json:"ip_allowlist_enabled" yaml:"ip_allowlist_enabled"`
	AllowedIPs         []string `json:"allowed_ips" yaml:"allowed_ips"`
	OpenPorts          []int32  `json:"open_ports" yaml:"open_ports"`
}

// Settings is the effective, persisted set of the three server-settings
// groups. It is what crosses the wire inside ServerInfo.
type Settings struct {
	Connection ServerConnection `json:"connection" yaml:"connection"`
	Alert      ServerAlert      `json:"alert" yaml:"alert"`
	Security   ServerSecurity   `json:"security" yaml:"security"`
}

// settingsManager owns the load-or-detect lifecycle for the effective
// settings. It is a struct (not a bare package-level sync.Once) so tests can
// construct isolated instances with a stubbed detector and a temporary path.
type settingsManager struct {
	once     sync.Once
	settings *Settings
	detect   func() *Settings // seam; default detectSettings
	pathFn   func() string    // seam; default settingsFilePath
}

// defaultSettings is the process-wide manager. Its once guarantees that
// detection (or the persisted-file load) runs exactly once per process.
var defaultSettings = &settingsManager{
	detect: detectSettings,
	pathFn: settingsFilePath,
}

// EnsureSettings loads (from disk) or detects the effective settings exactly
// once per process, persisting freshly-detected values. It never fails: any
// error is logged and the fallback defaults are returned, matching the
// best-effort nature of server.Initialize itself.
func EnsureSettings() *Settings {
	defaultSettings.ensure()
	return defaultSettings.settings
}

// GetSettings is an alias for EnsureSettings.
func GetSettings() *Settings {
	return EnsureSettings()
}

func (m *settingsManager) ensure() {
	m.once.Do(func() {
		if s := m.load(); s != nil {
			m.settings = s
			return
		}
		m.settings = m.detect()
		if err := m.persist(m.settings); err != nil {
			logs.Error("settings", "failed to persist server settings: %v", err)
		}
	})
}

// settingsFilePath resolves the settings file path lazily from the current
// environment (NOT from an init()-baked value) so callers and tests that
// override HOME get an isolated path.
func settingsFilePath() string {
	home := os.Getenv("HOME")
	if home == "" {
		home = os.Getenv("USERPROFILE") // Windows
	}
	return filepath.Join(home, ".phelix", serverSettingsFileName)
}

// load reads the persisted settings file. Returns nil when the file is
// missing or unparseable so the caller can fall back to detection.
func (m *settingsManager) load() *Settings {
	data, err := readFile(m.pathFn())
	if err != nil {
		return nil
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		logs.Warning("settings", "ignoring corrupt server settings file: %v", err)
		return nil
	}
	return &s
}

// persist writes the settings to disk with 0600 permissions. The explicit
// Chmod enforces the restrictive mode even when an older file already
// exists with looser permissions.
func (m *settingsManager) persist(s *Settings) error {
	path := m.pathFn()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, defaultSettingsFileMode); err != nil {
		return err
	}
	return os.Chmod(path, defaultSettingsFileMode)
}

// Function seams for unit tests (matching the repo's functional-type-seam
// pattern in internal/deploy). Detection is best-effort and never fatal:
// each detector returns the type's zero value on any error.
var (
	readFile          = os.ReadFile
	execOutput        = runCommand
	detectSSHPort     = detectSSHPortImpl
	detectSSHUser     = detectSSHUserImpl // real login user, not a product default
	detectAuthMethod  = detectAuthMethodImpl
	detectSSHKeys     = detectSSHKeysImpl // (privateKey, publicKey)
	detectFirewall    = detectFirewallImpl
	detectAutoUpdates = detectAutoUpdatesImpl
	detectSSHRoot     = detectSSHRootLoginImpl
	detectOpenPorts   = detectOpenPortsImpl
)

// runCommand runs a command and returns its trimmed stdout, or an error.
func runCommand(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// detectSettings composes the individual detectors and configured fallbacks
// into the effective settings. Never returns nil.
// alertFromConfig returns the agent's real alert configuration from the
// embedded config.yml. Zero thresholds (unset section) fall back to the
// built-in defaults so detection never reports literal zero.
func alertFromConfig() ServerAlert {
	a := config.Get()
	if a == nil {
		return ServerAlert{
			CPUThreshold:  defaultCPUThreshold,
			RAMThreshold:  defaultRAMThreshold,
			DiskThreshold: defaultDiskThreshold,
		}
	}
	cpu := a.Alert.CPUThreshold
	ram := a.Alert.RAMThreshold
	disk := a.Alert.DiskThreshold
	if cpu == 0 {
		cpu = defaultCPUThreshold
	}
	if ram == 0 {
		ram = defaultRAMThreshold
	}
	if disk == 0 {
		disk = defaultDiskThreshold
	}
	return ServerAlert{
		CPUThreshold:  cpu,
		RAMThreshold:  ram,
		DiskThreshold: disk,
	}
}

func detectSettings() *Settings {
	sshPort := detectSSHPort()
	if sshPort == 0 {
		sshPort = defaultSSHPort
	}

	authMethod := detectAuthMethod()
	if authMethod == "" {
		authMethod = defaultAuthMethod
	}

	privateKey, publicKey := detectSSHKeys()

	rootLogin := detectSSHRoot()
	if rootLogin == "" {
		rootLogin = defaultSSHRootLogin
	}

	return &Settings{
		Connection: ServerConnection{
			SSHPort:    sshPort,
			SSHUser:    detectSSHUser(),
			AuthMethod: authMethod,
			PrivateKey: privateKey,
			PublicKey:  publicKey,
		},
		Alert: alertFromConfig(),
		Security: ServerSecurity{
			FirewallEnabled: detectFirewall(),
			AutoUpdates:     detectAutoUpdates(),
			SSHRootLogin:    rootLogin,
			OpenPorts:       detectOpenPorts(),
		},
	}
}

// ============================================================================
// Detection
// ============================================================================

var (
	sshPortRe   = regexp.MustCompile(`(?i)^\s*Port\s+(\d+)\s*$`)
	rootLoginRe = regexp.MustCompile(`(?i)^\s*PermitRootLogin\s+(\S+)\s*$`)
	portRe      = regexp.MustCompile(`:(\d+)\s*$`)
)

// sshdConfigPaths are searched in order; drop-in files are read afterwards.
var sshdConfigPaths = []string{"/etc/ssh/sshd_config"}

// sshdConfigD is the drop-in directory whose *.conf files sshd includes.
const sshdConfigD = "/etc/ssh/sshd_config.d"

// readSSHDConfig returns the merged contents of the sshd configuration:
// the main file plus any *.conf drop-ins, mirroring sshd's default Include
// directive. Files are read in lexical order; missing paths are skipped.
func readSSHDConfig() string {
	seen := map[string]bool{}
	var buf strings.Builder

	for _, path := range sshdConfigPaths {
		if seen[path] {
			continue
		}
		seen[path] = true
		if data, err := readFile(path); err == nil {
			buf.Write(data)
			buf.WriteString("\n")
		}
	}

	if entries, err := os.ReadDir(sshdConfigD); err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || strings.HasPrefix(name, "RPM") || !strings.Contains(name, ".conf") {
				continue
			}
			path := filepath.Join(sshdConfigD, name)
			if seen[path] {
				continue
			}
			seen[path] = true
			if data, err := readFile(path); err == nil {
				buf.Write(data)
				buf.WriteString("\n")
			}
		}
	}

	return buf.String()
}

// configLines splits merged config text into trimmed, non-comment,
// non-blank lines.
func configLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// detectSSHUserImpl reports the real login user: $USER first, then a
// whoami fallback. Falls back to defaultSSHUser when neither yields a
// usable username (e.g. running under a service manager with no env).
func detectSSHUserImpl() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u, err := execOutput("whoami"); err == nil && u != "" {
		return u
	}
	return defaultSSHUser
}

// detectSSHPortImpl reads the configured sshd Port (sshd applies the
// first-obtained value). Falls back to 22.
func detectSSHPortImpl() int {
	for _, line := range configLines(readSSHDConfig()) {
		if m := sshPortRe.FindStringSubmatch(line); m != nil {
			if p, err := strconv.Atoi(m[1]); err == nil && p > 0 && p <= 65535 {
				return p
			}
		}
	}
	return defaultSSHPort
}

// detectSSHRootLoginImpl reads the sshd PermitRootLogin value. Legacy
// "without-password" is normalized to "prohibit-password". Falls back to
// "prohibit-password" (the modern OpenSSH default).
func detectSSHRootLoginImpl() string {
	for _, line := range configLines(readSSHDConfig()) {
		if m := rootLoginRe.FindStringSubmatch(line); m != nil {
			v := strings.ToLower(m[1])
			switch v {
			case "yes", "no", "prohibit-password":
				return v
			case "without-password":
				return "prohibit-password"
			}
			// Unknown values pass through to the caller's fallback.
		}
	}
	return defaultSSHRootLogin
}

// sshKeyCandidates are the client key files considered, in priority order.
var sshKeyCandidates = []string{"id_ed25519", "id_rsa", "id_ecdsa"}

// sshDirFromHome returns the SSH directory for the current user, honoring a
// HOME override for tests.
func sshDirFromHome() string {
	home := os.Getenv("HOME")
	if home == "" {
		home = os.Getenv("USERPROFILE") // Windows
	}
	return filepath.Join(home, ".ssh")
}

// findSSHKeyPath returns the path of the highest-priority existing client
// private key, or "".
func findSSHKeyPath() string {
	dir := sshDirFromHome()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	have := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			have[e.Name()] = true
		}
	}
	for _, name := range sshKeyCandidates {
		if have[name] {
			return filepath.Join(dir, name)
		}
	}
	return ""
}

// detectAuthMethodImpl reports the auth method ("key" or "password"). The
// CLI has no interactive password flow today, so auto-detection reports
// "key" when a client key exists and otherwise falls back to the product
// default "key"; it never invents a password.
func detectAuthMethodImpl() string {
	if findSSHKeyPath() != "" {
		return "key"
	}
	return defaultAuthMethod
}

// detectSSHKeysImpl reads the highest-priority private key present and its
// matching public key (.pub). Any read error produces empty strings so a
// half-parsed key is never reported.
func detectSSHKeysImpl() (privateKey, publicKey string) {
	path := findSSHKeyPath()
	if path == "" {
		return "", ""
	}
	data, err := readFile(path)
	if err != nil {
		return "", ""
	}
	pub := ""
	if d, err := readFile(path + ".pub"); err == nil {
		pub = string(d)
	}
	return string(data), pub
}

// detectFirewallImpl reports whether a host firewall appears active. It is a
// best-effort OR across the common tooling; returns false when none is
// present or reachable.
func detectFirewallImpl() bool {
	if out, err := execOutput("ufw", "status"); err == nil {
		if strings.Contains(out, "Status: active") {
			return true
		}
	}
	if out, err := execOutput("firewall-cmd", "--state"); err == nil {
		// firewalld prints exactly "running" when active (and "not running"
		// when inactive), so match the word, not a substring.
		if strings.EqualFold(out, "running") {
			return true
		}
	}
	if out, err := execOutput("iptables", "-S"); err == nil {
		// An active firewall has at least one -A rule; a bare empty table has
		// only chain definitions and policy lines.
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "-A ") {
				return true
			}
		}
	}
	if out, err := execOutput("nft", "list", "ruleset"); err == nil {
		if strings.Contains(out, " rule ") {
			return true
		}
	}
	return false
}

// autoUpdateTimers are the systemd timers commonly responsible for automatic
// package updates across distros.
var autoUpdateTimers = []string{
	"unattended-upgrades.timer",
	"apt-daily-upgrade.timer",
	"dnf-automatic.timer",
	"dnf-automatic-install.timer",
}

// detectAutoUpdatesImpl reports whether an automatic-update mechanism is
// enabled system-wide.
func detectAutoUpdatesImpl() bool {
	for _, timer := range autoUpdateTimers {
		if out, err := execOutput("systemctl", "is-enabled", timer); err == nil {
			switch out {
			case "enabled", "enabled-runtime", "static", "indirect":
				// static/indirect timers are always available; report them as
				// enabled too.
				return true
			}
		}
	}
	return false
}

// detectOpenPortsImpl reports the distinct listening TCP ports on the host.
// It parses `ss -tln` first, falling back to `netstat -tln`. Both missing →
// empty list. Best-effort; never fails.
func detectOpenPortsImpl() []int32 {
	ports := parseListeningPorts("ss", "-tln")
	if len(ports) == 0 {
		ports = parseListeningPorts("netstat", "-tln")
	}
	return ports
}

// parseListeningPorts runs a listener-listing command and extracts distinct,
// sorted ports from its output. Both `ss -tln` and `netstat -tln` place the
// local address in the 4th column; the port is the trailing number after the
// last ':' (including IPv6 [::]:port). The tools only emit listening TCP
// sockets, so the state/proto column needs no extra filtering.
func parseListeningPorts(name string, args ...string) []int32 {
	out, err := execOutput(name, args...)
	if err != nil {
		return nil
	}

	seen := map[int32]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		m := portRe.FindStringSubmatch(fields[3])
		if m == nil {
			continue
		}
		p, err := strconv.Atoi(m[1])
		if err != nil || p < 1 || p > 65535 {
			continue
		}
		seen[int32(p)] = true
	}

	ports := make([]int32, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return ports
}
