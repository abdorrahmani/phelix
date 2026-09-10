package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Detection
// ---------------------------------------------------------------------------

// withDetectors swaps the package-level detection seams for the duration of a
// test and restores them afterwards.
func withDetectors(t *testing.T, overrides map[string]any) {
	t.Helper()

	// Save the defaults.
	orig := map[string]any{
		"readFile":          readFile,
		"execOutput":        execOutput,
		"detectFirewall":    detectFirewall,
		"detectAutoUpdates": detectAutoUpdates,
		"detectSSHRoot":     detectSSHRoot,
		"detectOpenPorts":   detectOpenPorts,
	}
	t.Cleanup(func() {
		readFile = orig["readFile"].(func(string) ([]byte, error))
		execOutput = orig["execOutput"].(func(string, ...string) (string, error))
		detectFirewall = orig["detectFirewall"].(func() bool)
		detectAutoUpdates = orig["detectAutoUpdates"].(func() bool)
		detectSSHRoot = orig["detectSSHRoot"].(func() string)
		detectOpenPorts = orig["detectOpenPorts"].(func() []int32)
	})

	// Apply the overrides.
	if v, ok := overrides["readFile"]; ok {
		readFile = v.(func(string) ([]byte, error))
	}
	if v, ok := overrides["execOutput"]; ok {
		execOutput = v.(func(string, ...string) (string, error))
	}
	if v, ok := overrides["detectFirewall"]; ok {
		detectFirewall = v.(func() bool)
	}
	if v, ok := overrides["detectAutoUpdates"]; ok {
		detectAutoUpdates = v.(func() bool)
	}
	if v, ok := overrides["detectSSHRoot"]; ok {
		detectSSHRoot = v.(func() string)
	}
	if v, ok := overrides["detectOpenPorts"]; ok {
		detectOpenPorts = v.(func() []int32)
	}
}

func TestDetectSettings_Defaults(t *testing.T) {
	withDetectors(t, map[string]any{
		"detectFirewall":    func() bool { return false },
		"detectAutoUpdates": func() bool { return false },
		"detectSSHRoot":     func() string { return "" }, // "" → caller falls back to "prohibit-password"
		"detectOpenPorts":   func() []int32 { return nil },
	})

	s := detectSettings()
	if s == nil {
		t.Fatal("detectSettings must never return nil")
	}
	if s.Connection != (ServerConnection{}) {
		t.Fatalf("connection must never be populated, got %+v", s.Connection)
	}
	if s.Alert.CPUThreshold != 80 || s.Alert.RAMThreshold != 90 || s.Alert.DiskThreshold != 90 {
		t.Fatalf("alert thresholds mismatch: %+v", s.Alert)
	}
	if s.Alert.CPUSpikeAlerts || s.Alert.MemoryPressureAlerts || s.Alert.DiskSpaceAlerts ||
		s.Alert.AppCrashAlerts || s.Alert.AgentDisconnectAlerts || s.Alert.WeeklyDigest {
		t.Fatalf("expected all alert toggles false: %+v", s.Alert)
	}
	if s.Security.FirewallEnabled || s.Security.AutoUpdates {
		t.Fatalf("expected firewall/auto-updates false: %+v", s.Security)
	}
	if s.Security.SSHRootLogin != "prohibit-password" || s.Security.IPAllowlistEnabled {
		t.Fatalf("security defaults mismatch: %+v", s.Security)
	}
	if len(s.Security.AllowedIPs) != 0 || len(s.Security.OpenPorts) != 0 {
		t.Fatalf("expected empty allowed_ips/open_ports: %+v", s.Security)
	}
}

// TestDetectSettings_NeverCollectsSSHDetails pins the security contract: even
// with a full SSH keypair and an sshd Port in place, detection reports nothing
// about the host's SSH configuration.
func TestDetectSettings_NeverCollectsSSHDetails(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("USER", "deploy")

	sshDir := filepath.Join(tmp, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519"), []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte("PUBLIC"), 0o644); err != nil {
		t.Fatal(err)
	}

	withDetectors(t, map[string]any{
		"readFile": func(path string) ([]byte, error) {
			if path == "/etc/ssh/sshd_config" {
				return []byte("Port 2222\nPermitRootLogin no\n"), nil
			}
			return os.ReadFile(path)
		},
	})

	s := detectSettings()
	if s.Connection != (ServerConnection{}) {
		t.Fatalf("SSH details leaked into connection: %+v", s.Connection)
	}
	// The sshd file is still read for the Security group.
	if s.Security.SSHRootLogin != "no" {
		t.Fatalf("expected ssh_root_login no, got %q", s.Security.SSHRootLogin)
	}
}

func TestDetectSettings_DetectionValues(t *testing.T) {
	withDetectors(t, map[string]any{
		"detectFirewall":    func() bool { return true },
		"detectAutoUpdates": func() bool { return true },
		"detectSSHRoot":     func() string { return "yes" },
		"detectOpenPorts":   func() []int32 { return []int32{22, 443} },
	})

	s := detectSettings()
	if !s.Security.FirewallEnabled || !s.Security.AutoUpdates || s.Security.SSHRootLogin != "yes" {
		t.Fatalf("security mismatch: %+v", s.Security)
	}
	if !reflect.DeepEqual(s.Security.OpenPorts, []int32{22, 443}) {
		t.Fatalf("open_ports mismatch: %v", s.Security.OpenPorts)
	}
}

func TestAlertFromConfig_FallsBackWhenUnset(t *testing.T) {
	// config.Get() is nil (Load never ran under go test) → built-in defaults.
	a := alertFromConfig()
	if a.CPUThreshold != defaultCPUThreshold || a.RAMThreshold != defaultRAMThreshold || a.DiskThreshold != defaultDiskThreshold {
		t.Fatalf("expected fallback thresholds, got %+v", a)
	}
}

func TestDetectSSHRootLogin_Parsing(t *testing.T) {
	cases := []struct {
		config string
		want   string
	}{
		{"PermitRootLogin no", "no"},
		{"PermitRootLogin yes", "yes"},
		{"PermitRootLogin prohibit-password", "prohibit-password"},
		{"PermitRootLogin without-password", "prohibit-password"}, // legacy synonym
		{"Port 22\nPermitRootLogin no", "no"},
		{"#PermitRootLogin yes\nPermitRootLogin no", "no"}, // comments ignored
		{"", "prohibit-password"},                          // absent → modern default
	}

	for _, tc := range cases {
		cfg := tc.config
		withDetectors(t, map[string]any{
			"readFile": func(path string) ([]byte, error) {
				if path == "/etc/ssh/sshd_config" {
					return []byte(cfg), nil
				}
				return nil, os.ErrNotExist
			},
		})
		if got := detectSSHRootLoginImpl(); got != tc.want {
			t.Fatalf("config %q: expected %q, got %q", tc.config, tc.want, got)
		}
	}
}

func TestDetectFirewall(t *testing.T) {
	cases := []struct {
		name    string
		outputs map[string]string // "<name>" → command output
		errs    map[string]bool   // command → error
		want    bool
	}{
		{
			name:    "ufw active",
			outputs: map[string]string{"ufw status": "Status: active\n"},
			want:    true,
		},
		{
			name:    "firewalld running",
			outputs: map[string]string{"firewall-cmd --state": "running\n"},
			want:    true,
		},
		{
			name:    "iptables has rules",
			outputs: map[string]string{"iptables -S": "-P INPUT ACCEPT\n-A INPUT -p tcp --dport 22 -j ACCEPT\n"},
			want:    true,
		},
		{
			name:    "nftables has rules",
			outputs: map[string]string{"nft list ruleset": "table inet filter { chain input { rule ip saddr 1.2.3.4 drop } }\n"},
			want:    true,
		},
		{
			name: "nothing active",
			outputs: map[string]string{
				"ufw status":           "Status: inactive\n",
				"firewall-cmd --state": "not running\n",
				"iptables -S":          "-P INPUT ACCEPT\n",
			},
			want: false,
		},
		{
			name: "all tools missing",
			errs: map[string]bool{"ufw status": true, "firewall-cmd --state": true, "iptables -S": true, "nft list ruleset": true},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withDetectors(t, map[string]any{
				"execOutput": func(name string, args ...string) (string, error) {
					cmd := name + " " + strings.Join(args, " ")
					if tc.errs[cmd] {
						return "", &execError{cmd}
					}
					out, ok := tc.outputs[cmd]
					if !ok {
						return "", &execError{cmd}
					}
					return strings.TrimSpace(out), nil
				},
			})
			if got := detectFirewallImpl(); got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

// execError is a minimal error for stubbed command failures.
type execError struct{ cmd string }

func (e *execError) Error() string { return "command not found: " + e.cmd }

func TestDetectAutoUpdates(t *testing.T) {
	withDetectors(t, map[string]any{
		"execOutput": func(name string, args ...string) (string, error) {
			if name == "systemctl" && len(args) == 2 && args[0] == "is-enabled" {
				if args[1] == "unattended-upgrades.timer" {
					return "enabled", nil
				}
				return "disabled", nil
			}
			return "", &execError{"missing"}
		},
	})
	if !detectAutoUpdatesImpl() {
		t.Fatal("expected auto_updates true when unattended-upgrades.timer is enabled")
	}

	withDetectors(t, map[string]any{
		"execOutput": func(name string, args ...string) (string, error) {
			return "disabled", nil
		},
	})
	if detectAutoUpdatesImpl() {
		t.Fatal("expected auto_updates false when no timer is enabled")
	}
}

func TestParseListeningPorts(t *testing.T) {
	withDetectors(t, map[string]any{
		"execOutput": func(name string, args ...string) (string, error) {
			// Both ss and netstat shapes.
			return "LISTEN 0 128 0.0.0.0:22 0.0.0.0:*\n" +
				"LISTEN 0 128 [::]:443 [::]:*\n" +
				"LISTEN 0 128 127.0.0.1:50051 127.0.0.1:*\n" +
				"LISTEN 0 128 10.0.0.5:9090 0.0.0.0:*\n", nil
		},
	})

	got := parseListeningPorts("ss", "-tln")
	want := []int32{22, 443, 9090, 50051}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestParseListeningPorts_CommandFails(t *testing.T) {
	withDetectors(t, map[string]any{
		"execOutput": func(name string, args ...string) (string, error) {
			return "", &execError{"missing"}
		},
	})
	if got := parseListeningPorts("ss", "-tln"); len(got) != 0 {
		t.Fatalf("expected empty ports, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Manager (load-or-detect + persistence)
// ---------------------------------------------------------------------------

func TestManager_LoadOrDetect_NoFileDetectsAndPersists(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "settings.json")

	calls := 0
	m := &settingsManager{
		detect: func() *Settings {
			calls++
			return detectSettings()
		},
		pathFn: func() string { return path },
	}

	withDetectors(t, map[string]any{
		"detectSSHRoot": func() string { return "no" },
	})

	m.ensure()
	if calls != 1 {
		t.Fatalf("expected one detect call, got %d", calls)
	}
	if m.settings == nil || m.settings.Security.SSHRootLogin != "no" {
		t.Fatalf("unexpected settings: %+v", m.settings)
	}
	if m.settings.Connection != (ServerConnection{}) {
		t.Fatalf("expected empty connection, got %+v", m.settings.Connection)
	}

	// File must exist with 0600 permissions.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected settings file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600, got %v", info.Mode().Perm())
	}
}

func TestManager_LoadOrDetect_PersistedFileWins(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "settings.json")

	// Pre-write a settings file with custom values.
	input := Settings{
		Alert:    ServerAlert{CPUThreshold: 70},
		Security: ServerSecurity{SSHRootLogin: "no"},
	}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	detectCalled := false
	m := &settingsManager{
		detect: func() *Settings {
			detectCalled = true
			return &Settings{}
		},
		pathFn: func() string { return path },
	}

	m.ensure()
	if detectCalled {
		t.Fatal("expected detection NOT to run when a persisted file exists")
	}
	if m.settings == nil || m.settings.Alert.CPUThreshold != 70 || m.settings.Security.SSHRootLogin != "no" {
		t.Fatalf("expected loaded settings, got %+v", m.settings)
	}
}

// TestManager_ScrubsPersistedConnection covers the upgrade path: a
// settings.json written by an older build still carries the operator's SSH
// details, and ensure() must drop them from memory AND rewrite the file so the
// key material leaves the disk.
func TestManager_ScrubsPersistedConnection(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "settings.json")

	stale := Settings{
		Connection: ServerConnection{
			SSHPort:    2222,
			SSHUser:    "deploy",
			AuthMethod: "key",
			PrivateKey: "PRIVATE-KEY-MATERIAL",
			PublicKey:  "PUBLIC-KEY",
		},
		Alert:    ServerAlert{CPUThreshold: 70},
		Security: ServerSecurity{SSHRootLogin: "no"},
	}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	m := &settingsManager{
		detect: func() *Settings { t.Fatal("detection must not run"); return nil },
		pathFn: func() string { return path },
	}
	m.ensure()

	if m.settings.Connection != (ServerConnection{}) {
		t.Fatalf("connection not scrubbed: %+v", m.settings.Connection)
	}
	// The other groups survive the scrub.
	if m.settings.Alert.CPUThreshold != 70 || m.settings.Security.SSHRootLogin != "no" {
		t.Fatalf("scrub clobbered other groups: %+v", m.settings)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected rewritten settings file: %v", err)
	}
	for _, secret := range []string{"PRIVATE-KEY-MATERIAL", "PUBLIC-KEY", "deploy", "2222"} {
		if strings.Contains(string(onDisk), secret) {
			t.Fatalf("%q still on disk after scrub: %s", secret, onDisk)
		}
	}
}

func TestManager_LoadOrDetect_CorruptFileFallsBackAndRewrites(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "settings.json")
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	withDetectors(t, map[string]any{
		"detectSSHRoot": func() string { return "no" },
	})

	m := &settingsManager{
		detect: detectSettings,
		pathFn: func() string { return path },
	}
	m.ensure()

	if m.settings == nil || m.settings.Security.SSHRootLogin != "no" {
		t.Fatalf("expected detected settings after corrupt file, got %+v", m.settings)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected rewritten settings file: %v", err)
	}
	if !strings.Contains(string(data), `"ssh_root_login": "no"`) {
		t.Fatalf("expected rewritten file to contain the detected value: %q", string(data))
	}
	if !strings.Contains(string(data), `"ssh_user": ""`) {
		t.Fatalf("expected rewritten file to carry an empty ssh_user: %q", string(data))
	}
}

func TestManager_PersistEnforces0600OverExistingMode(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "settings.json")
	// Simulate a previously-written file with loose permissions.
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &settingsManager{
		detect: detectSettings,
		pathFn: func() string { return path },
	}
	if err := m.persist(detectSettings()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 after persist over a 0644 file, got %v", info.Mode().Perm())
	}
}

func TestManager_EnsureIsOncePerManager(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "settings.json")

	calls := 0
	m := &settingsManager{
		detect: func() *Settings {
			calls++
			return detectSettings()
		},
		pathFn: func() string { return path },
	}

	m.ensure()
	m.ensure()
	if calls != 1 {
		t.Fatalf("expected exactly one detect call across ensure() calls, got %d", calls)
	}
	if m.settings == nil {
		t.Fatal("expected settings to be populated")
	}
}
