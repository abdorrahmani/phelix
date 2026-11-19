package update

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestResolveLatestChannels(t *testing.T) {
	for _, tc := range []struct {
		name, channel, version, path string
		header                       bool
		code                         phelixerr.Code
	}{
		{name: "default stable", version: "1.2.3", path: "/releases/latest/version"},
		{name: "explicit stable", channel: "stable", version: "1.2.3", path: "/releases/latest/version"},
		{name: "beta body", channel: "beta", version: "1.3.0-beta.1", path: "/releases/beta/version"},
		{name: "beta header", channel: "beta", version: "v1.3.0-beta.2", path: "/releases/beta/version", header: true},
		{name: "bare beta", channel: "beta", version: "1.3.0-beta", path: "/releases/beta/version"},
		{name: "stable rejects beta", version: "1.3.0-beta.1", path: "/releases/latest/version", code: phelixerr.CodeConfiguration},
		{name: "stable rejects dev", version: "1.3.0dev", path: "/releases/latest/version", code: phelixerr.CodeConfiguration},
		{name: "beta rejects stable", channel: "beta", version: "1.3.0", path: "/releases/beta/version", code: phelixerr.CodeConfiguration},
		{name: "beta rejects other prereleases", channel: "beta", version: "1.3.0-rc.1", path: "/releases/beta/version", code: phelixerr.CodeConfiguration},
		{name: "malformed version", channel: "beta", version: "../latest", path: "/releases/beta/version", code: phelixerr.CodeConfiguration},
		{name: "unknown channel", channel: "nightly", code: phelixerr.CodeInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path != tc.path {
					t.Errorf("path = %q, want %q", r.URL.Path, tc.path)
				}
				if tc.header {
					w.Header().Set(versionHeader, tc.version)
					fmt.Fprintln(w, "1.2.3")
					return
				}
				fmt.Fprintln(w, tc.version)
				fmt.Fprintln(w, "additional metadata")
			}))
			defer srv.Close()
			got, err := (&ReleaseClient{BaseURL: srv.URL, Channel: tc.channel}).ResolveLatest()
			if tc.code != "" {
				if !phelixerr.IsCode(err, tc.code) {
					t.Fatalf("error = %v, want %s", err, tc.code)
				}
			} else if err != nil || got != tc.version {
				t.Fatalf("ResolveLatest() = %q, %v; want %q", got, err, tc.version)
			}
			if tc.path == "" && requests != 0 {
				t.Fatal("unknown channel contacted the release server")
			}
		})
	}
}

func TestBetaChannelMissingDoesNotUseStable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/beta/version" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	_, err := (&ReleaseClient{BaseURL: srv.URL, Channel: "beta"}).ResolveLatest()
	if !phelixerr.IsCode(err, phelixerr.CodeNotFound) || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("error = %v, want missing beta release", err)
	}
}

func TestBetaArtifactUsesVersionedDirectory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/1.3.0-beta.1/phelix-linux-amd64" {
			t.Errorf("unexpected artifact path: %s", r.URL.Path)
		}
		fmt.Fprint(w, "beta binary")
	}))
	defer srv.Close()
	client := &ReleaseClient{BaseURL: srv.URL, Channel: "beta"}
	if err := client.DownloadArtifact("v1.3.0-beta.1", Platform{"linux", "amd64"}, filepath.Join(t.TempDir(), "binary")); err != nil {
		t.Fatal(err)
	}
}

func TestRunBetaVersionOrdering(t *testing.T) {
	for _, tc := range []struct {
		current, latest string
		outcome         Outcome
	}{
		{"1.2.3", "1.3.0-beta.1", OutcomeAvailable},
		{"1.3.0-beta.2", "1.3.0-beta.10", OutcomeAvailable},
		{"1.3.0-beta.10", "1.3.0-beta.10", OutcomeUpToDate},
		{"1.3.0", "1.3.0-beta.10", OutcomeLocalNewer},
		{"1.3.0-beta.10", "1.3.0-beta.2", OutcomeLocalNewer},
	} {
		t.Run(tc.current+" to "+tc.latest, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/releases/beta/version" {
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				fmt.Fprintln(w, tc.latest)
			}))
			defer srv.Close()
			target := newUpdateTarget(t)
			res, err := Run(Options{BaseURL: srv.URL, Channel: "beta", CurrentVersion: tc.current, CheckOnly: true, TargetPath: target, Runner: missingRunner()})
			if err != nil || res.Outcome != tc.outcome {
				t.Fatalf("Run() = %+v, %v; want outcome %v", res, err, tc.outcome)
			}
			if string(readFile(t, target)) != "OLD-PHELIX-BINARY" {
				t.Fatal("check replaced the binary")
			}
		})
	}
}
