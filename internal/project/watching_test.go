package project

import (
	"strings"
	"testing"
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
