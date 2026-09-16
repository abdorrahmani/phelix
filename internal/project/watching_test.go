package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// The top-level watching key accepts exactly enable/disable; anything else
// fails validation with a hint naming the expected values.
func TestLoadWatchingValues(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string // expected Watching value; "" = absent
		wantErr string
	}{
		{"enable", "name: api\nwatching: enable\n", WatchingEnable, ""},
		{"disable", "name: api\nwatching: disable\n", WatchingDisable, ""},
		{"absent", "name: api\n", "", ""},
		{"empty value", "name: api\nwatching: \"\"\n", "", ""},
		{"typo enabled", "name: api\nwatching: enabled\n", "", `invalid watching "enabled"`},
		{"boolean true", "name: api\nwatching: true\n", "", `invalid watching "true"`},
		{"bare word", "name: api\nwatching: maybe\n", "", `invalid watching "maybe"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(write(t, tc.content))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("invalid watching accepted:\n%s", tc.content)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Watching != tc.want {
				t.Errorf("Watching = %q, want %q", cfg.Watching, tc.want)
			}
		})
	}
}

// WatchingSetting is the presence-aware accessor the build/rebuild sync uses:
// enable → (true, true), disable → (false, true), absent → (_, false).
func TestWatchingSetting(t *testing.T) {
	cases := []struct {
		watching    string
		wantEnabled bool
		wantOK      bool
	}{
		{WatchingEnable, true, true},
		{WatchingDisable, false, true},
		{"", false, false},
	}
	for _, tc := range cases {
		cfg := &Config{Watching: tc.watching}
		enabled, ok := cfg.WatchingSetting()
		if ok != tc.wantOK || (ok && enabled != tc.wantEnabled) {
			t.Errorf("WatchingSetting(%q) = (%v, %v), want (%v, %v)",
				tc.watching, enabled, ok, tc.wantEnabled, tc.wantOK)
		}
	}
}

// A nil *Config means no phelix.yaml was loaded (loadProjectConfig returns
// (nil, nil) for a missing file). WatchingSetting must report the key as
// absent instead of panicking — `phelix build` in a config-less directory
// calls it on every run.
func TestWatchingSettingNilConfig(t *testing.T) {
	var cfg *Config
	enabled, ok := cfg.WatchingSetting()
	if ok {
		t.Fatal("nil config must report the watching key as absent")
	}
	if enabled {
		t.Fatal("nil config must not report watching as enabled")
	}
}

// A saved watching value must round-trip through Save/Load — `phelix init`
// writes the disable default and later matrix-init merges must not lose it.
func TestWatchingRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{Name: "api", Port: 4000, Watching: WatchingDisable}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Watching != WatchingDisable {
		t.Fatalf("Watching round-trip = %q, want %q", got.Watching, WatchingDisable)
	}
	if _, ok := got.WatchingSetting(); !ok {
		t.Fatal("saved disable must round-trip as a present key")
	}
}

// SetWatching is the yaml half of `phelix watch`: it must replace (or append)
// only the watching value, preserving every other key and comment, and must
// never create or corrupt the file.
func TestSetWatching(t *testing.T) {
	const initStyle = `# Phelix project configuration.
name: api
port: 3000
watching: disable
`
	t.Run("replaces existing key and preserves the rest", func(t *testing.T) {
		dir := write(t, initStyle)
		if err := SetWatching(dir, true); err != nil {
			t.Fatalf("SetWatching(enable): %v", err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, FileName))
		if err != nil {
			t.Fatal(err)
		}
		got := string(raw)
		for _, want := range []string{"# Phelix project configuration.", "name: api", "port: 3000", "watching: enable"} {
			if !strings.Contains(got, want) {
				t.Fatalf("updated yaml missing %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "disable") {
			t.Fatalf("old watching value survived:\n%s", got)
		}
		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("Load after SetWatching: %v", err)
		}
		if enabled, ok := cfg.WatchingSetting(); !ok || !enabled {
			t.Fatalf("round-trip WatchingSetting = (%v, %v), want (true, true)", enabled, ok)
		}
	})

	t.Run("appends when the key is absent", func(t *testing.T) {
		dir := write(t, "name: api\nport: 3000\n")
		if err := SetWatching(dir, false); err != nil {
			t.Fatalf("SetWatching(disable): %v", err)
		}
		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("Load after SetWatching: %v", err)
		}
		if enabled, ok := cfg.WatchingSetting(); !ok || enabled {
			t.Fatalf("round-trip WatchingSetting = (%v, %v), want (false, true)", enabled, ok)
		}
	})

	t.Run("missing file is reported, never created", func(t *testing.T) {
		dir := t.TempDir()
		err := SetWatching(dir, true)
		if phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
			t.Fatalf("SetWatching on missing file = %v, want CodeNotFound", err)
		}
		if _, serr := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(serr) {
			t.Fatal("SetWatching created a phelix.yaml; that is phelix init's job")
		}
	})

	t.Run("malformed file is an error and is left unmodified", func(t *testing.T) {
		const broken = "name: api\n\twatching: [oops\n"
		dir := write(t, broken)
		before, _ := os.ReadFile(filepath.Join(dir, FileName))
		err := SetWatching(dir, true)
		if phelixerr.CodeOf(err) != phelixerr.CodeConfiguration {
			t.Fatalf("SetWatching on malformed file = %v, want CodeConfiguration", err)
		}
		after, _ := os.ReadFile(filepath.Join(dir, FileName))
		if string(before) != string(after) {
			t.Fatal("malformed phelix.yaml was modified despite the error")
		}
	})
}
