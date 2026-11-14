package hostsecurity

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// primeCache seeds the intelligence cache with a fixture advisory body so
// matchers read a loaded coverage set without network access.
func primeCache(t *testing.T, intel *Intelligence, address string, body []byte) {
	t.Helper()
	sum := sha256.Sum256([]byte(address))
	digest := sha256.Sum256(body)
	cached := struct {
		Source Source
		Body   []byte
	}{Source: Source{Name: "fixture", URL: address, RetrievedAt: time.Now().UTC(), Digest: hex.EncodeToString(digest[:])}, Body: body}
	raw, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intel.CacheDir, hex.EncodeToString(sum[:])+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// unavailableHandler rejects every enrichment request with HTTP 404 so the
// enrichment loop fails fast without retries.
type unavailableHandler struct{}

func (unavailableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not available", http.StatusNotFound)
}

func TestDebianUnknownPackages(t *testing.T) {
	tracker := map[string]any{
		"ssl":     map[string]any{"CVE-2024-10001": map[string]any{"releases": map[string]any{"bookworm": map[string]any{"status": "open", "fixed_version": "", "urgency": "high"}}}},
		"phantom": map[string]any{"CVE-2024-10002": map[string]any{"releases": map[string]any{"bullseye": map[string]any{"status": "open", "fixed_version": ""}}}},
	}
	raw, err := json.Marshal(tracker)
	if err != nil {
		t.Fatal(err)
	}
	intel := &Intelligence{CacheDir: t.TempDir()}
	primeCache(t, intel, "https://security-tracker.debian.org/tracker/data/json", raw)
	packages := []Package{
		{Key: "ssl:amd64@1.0-1", Name: "ssl", Version: "1.0-1", SourceVersion: "1.0-1", Architecture: "amd64", Source: "ssl"},
		{Key: "gone:amd64@2.0-1", Name: "gone", Version: "2.0-1", SourceVersion: "2.0-1", Architecture: "amd64", Source: "gone"},
		{Key: "phantom:amd64@3.0-1", Name: "phantom", Version: "3.0-1", SourceVersion: "3.0-1", Architecture: "amd64", Source: "phantom"},
	}
	findings, _, unknown, err := intel.debian(context.Background(), System{}, Host{ID: "debian", Version: "12", Status: "supported"}, packages)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].CVE != "CVE-2024-10001" || findings[0].Severity != "high" {
		t.Fatalf("unexpected findings: %+v", findings)
	}
	if unknown != 2 {
		t.Fatalf("unknown = %d, want 2 (absent source and missing release mapping)", unknown)
	}
}

func TestAlpineUnknownPackages(t *testing.T) {
	main, err := json.Marshal(map[string]any{"distroversion": "v3.19", "packages": []any{map[string]any{"pkg": map[string]any{"name": "ssl", "secfixes": map[string]any{"3.19.1-r0": []string{"CVE-2024-10003"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	community, err := json.Marshal(map[string]any{"distroversion": "v3.19", "packages": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	intel := &Intelligence{CacheDir: t.TempDir()}
	primeCache(t, intel, "https://secdb.alpinelinux.org/v3.19/main.json", main)
	primeCache(t, intel, "https://secdb.alpinelinux.org/v3.19/community.json", community)
	packages := []Package{
		{Key: "ssl:x86_64@1.0-r0", Name: "ssl", Version: "1.0-r0", Architecture: "x86_64", Source: "ssl"},
		{Key: "lonely:x86_64@1.0-r0", Name: "lonely", Version: "1.0-r0", Architecture: "x86_64", Source: "lonely"},
		{Key: "stray:x86_64@1.0-r0", Name: "stray", Version: "1.0-r0", Architecture: "x86_64", Source: "stray"},
	}
	system := System{Runner: fakeRunner(func(context.Context, string, ...string) ([]byte, error) { return []byte("<"), nil })}
	findings, _, unknown, err := intel.alpine(context.Background(), system, Host{ID: "alpine", Version: "3.19.4", Manager: "apk", Status: "supported"}, packages)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].CVE != "CVE-2024-10003" || findings[0].FixedVersion != "3.19.1-r0" {
		t.Fatalf("unexpected findings: %+v", findings)
	}
	if unknown != 2 {
		t.Fatalf("unknown = %d, want 2", unknown)
	}
}

func TestArchUnknownPackages(t *testing.T) {
	all, err := json.Marshal([]any{map[string]any{"name": "AVG-1", "packages": []string{"ssl"}, "status": "Vulnerable", "severity": "high", "affected": "1.0", "fixed": "2.0", "issues": []string{"CVE-2024-10004"}}})
	if err != nil {
		t.Fatal(err)
	}
	intel := &Intelligence{CacheDir: t.TempDir()}
	primeCache(t, intel, "https://security.archlinux.org/all.json", all)
	packages := []Package{
		{Key: "ssl:x86_64@1.0-1", Name: "ssl", Version: "1.0-1", Architecture: "x86_64", Source: "ssl"},
		{Key: "lonely:x86_64@1.0-1", Name: "lonely", Version: "1.0-1", Architecture: "x86_64", Source: "lonely"},
	}
	system := System{Runner: fakeRunner(func(context.Context, string, ...string) ([]byte, error) { return []byte("-1"), nil })}
	findings, _, unknown, err := intel.arch(context.Background(), system, Host{ID: "arch", Version: "rolling", Manager: "pacman", Status: "supported"}, packages)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].CVE != "CVE-2024-10004" || findings[0].Severity != "high" {
		t.Fatalf("unexpected findings: %+v", findings)
	}
	if unknown != 1 {
		t.Fatalf("unknown = %d, want 1", unknown)
	}
}

func TestUbuntuUnknownPackages(t *testing.T) {
	database, err := json.Marshal(map[string]any{"6969-1": map[string]any{"cves": []string{"CVE-2024-10005"}, "releases": map[string]any{"noble": map[string]any{"binaries": map[string]any{"ssl": map[string]any{"version": "2.0-1"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	intel := &Intelligence{CacheDir: t.TempDir()}
	primeCache(t, intel, "https://usn.ubuntu.com/usn-db/database.json.bz2", database)
	packages := []Package{
		{Key: "ssl:amd64@1.0-1", Name: "ssl", Version: "1.0-1", Architecture: "amd64", Source: "ssl"},
		{Key: "lonely:amd64@1.0-1", Name: "lonely", Version: "1.0-1", Architecture: "amd64", Source: "lonely"},
	}
	findings, _, unknown, err := intel.ubuntu(context.Background(), System{}, Host{ID: "ubuntu", Version: "24.04", Codename: "noble", Status: "supported"}, packages)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].CVE != "CVE-2024-10005" || findings[0].AdvisoryID != "USN-6969-1" {
		t.Fatalf("unexpected findings: %+v", findings)
	}
	if unknown != 1 {
		t.Fatalf("unknown = %d, want 1", unknown)
	}
}

func TestDebianFixedVersionVocabulary(t *testing.T) {
	tracker := map[string]any{
		"openzero":      map[string]any{"CVE-2024-20001": map[string]any{"releases": map[string]any{"bookworm": map[string]any{"status": "open", "fixed_version": "0", "urgency": "high"}}}},
		"resolvedzero":  map[string]any{"CVE-2024-20002": map[string]any{"releases": map[string]any{"bookworm": map[string]any{"status": "resolved", "fixed_version": "0", "urgency": "high"}}}},
		"notaffected":   map[string]any{"CVE-2024-20003": map[string]any{"releases": map[string]any{"bookworm": map[string]any{"status": "not-affected", "fixed_version": "", "urgency": "high"}}}},
		"wordfix":       map[string]any{"CVE-2024-20004": map[string]any{"releases": map[string]any{"bookworm": map[string]any{"status": "open", "fixed_version": "not-affected", "urgency": "medium"}}}},
		"strangestatus": map[string]any{"CVE-2024-20005": map[string]any{"releases": map[string]any{"bookworm": map[string]any{"status": "mysterious", "fixed_version": "2.0-1", "urgency": "low"}}}},
		"comparable":    map[string]any{"CVE-2024-20006": map[string]any{"releases": map[string]any{"bookworm": map[string]any{"status": "open", "fixed_version": "2.0-1", "urgency": "high**"}}}},
	}
	raw, err := json.Marshal(tracker)
	if err != nil {
		t.Fatal(err)
	}
	intel := &Intelligence{CacheDir: t.TempDir()}
	primeCache(t, intel, "https://security-tracker.debian.org/tracker/data/json", raw)
	packages := []Package{}
	for _, name := range []string{"openzero", "resolvedzero", "notaffected", "wordfix", "strangestatus", "comparable"} {
		packages = append(packages, Package{Key: name + ":amd64@1.0-1", Name: name, Version: "1.0-1", SourceVersion: "1.0-1", Architecture: "amd64", Source: name})
	}
	findings, _, unknown, err := intel.debian(context.Background(), System{}, Host{ID: "debian", Version: "12", Status: "supported"}, packages)
	if err != nil {
		t.Fatal(err)
	}
	if unknown != 0 {
		t.Fatalf("unknown = %d, want 0: every source has a release entry", unknown)
	}
	byPackage := map[string][]Finding{}
	for _, f := range findings {
		byPackage[f.Package] = append(byPackage[f.Package], f)
	}
	if got := byPackage["openzero"]; len(got) != 1 || got[0].State != "unverified" || got[0].Severity != "high" ||
		got[0].FixedVersion != "" || got[0].AffectedRange != "distribution advisory status" {
		t.Fatalf("open with fixed 0 must yield one unverified finding without a comparison: %+v", got)
	}
	if got := byPackage["resolvedzero"]; len(got) != 0 {
		t.Fatalf("resolved with fixed 0 is not affected and must stay silent: %+v", got)
	}
	if got := byPackage["notaffected"]; len(got) != 0 {
		t.Fatalf("not-affected must stay silent: %+v", got)
	}
	if got := byPackage["wordfix"]; len(got) != 1 || got[0].State != "unverified" || got[0].FixedVersion != "" {
		t.Fatalf("a word fixed_version must never reach the comparator: %+v", got)
	}
	if got := byPackage["strangestatus"]; len(got) != 1 || got[0].State != "unverified" {
		t.Fatalf("an unknown status must degrade to unverified without a comparison: %+v", got)
	}
	if got := byPackage["comparable"]; len(got) != 1 || got[0].State != "confirmed" || got[0].Severity != "high" || got[0].FixedVersion != "2.0-1" {
		t.Fatalf("a real fixed version must compare: %+v", got)
	}
}

func TestUbuntuEmptyBinaryVersionIsUnknown(t *testing.T) {
	database, err := json.Marshal(map[string]any{"6969-1": map[string]any{
		"cves":     []string{"CVE-2024-20007"},
		"releases": map[string]any{"noble": map[string]any{"binaries": map[string]any{"ssl": map[string]any{"version": ""}, "settled": map[string]any{"version": "2.0-1"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	intel := &Intelligence{CacheDir: t.TempDir()}
	primeCache(t, intel, "https://usn.ubuntu.com/usn-db/database.json.bz2", database)
	packages := []Package{
		{Key: "ssl:amd64@1.0-1", Name: "ssl", Version: "1.0-1", Architecture: "amd64", Source: "ssl"},
		{Key: "settled:amd64@2.0-1", Name: "settled", Version: "2.0-1", Architecture: "amd64", Source: "settled"},
	}
	findings, _, unknown, err := intel.ubuntu(context.Background(), System{}, Host{ID: "ubuntu", Version: "24.04", Codename: "noble", Status: "supported"}, packages)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("no notice carries a usable vulnerable encoding: %+v", findings)
	}
	if unknown != 1 {
		t.Fatalf("unknown = %d, want 1: the empty binary version leaves the package undetermined", unknown)
	}
}

func TestRPMAdvisoriesZeroUnknown(t *testing.T) {
	intel := &Intelligence{CacheDir: t.TempDir()}
	packages := []Package{{Key: "openssl:x86_64@1:3.0.7-27.el9", Name: "openssl", Version: "1:3.0.7-27.el9", Architecture: "x86_64", Source: "openssl"}}
	_, _, unknown, err := intel.rpmAdvisories(context.Background(), System{Root: t.TempDir()}, Host{ID: "fedora", Version: "39", Manager: "dnf", Status: "supported"}, packages)
	if err == nil {
		t.Fatal("expected failure without repository metadata")
	}
	if unknown != 0 {
		t.Fatalf("unknown = %d, want 0 when no coverage set was loaded", unknown)
	}
}

// blockedHandler holds every request until the request context is
// cancelled, standing in for an unreachable enrichment provider on a
// firewalled host.
type blockedHandler struct{}

func (blockedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	<-r.Context().Done()
}

func TestScanHostEnrichmentBudgetExpiresCleanly(t *testing.T) {
	tracker := map[string]any{
		"ssl": map[string]any{"CVE-2024-10001": map[string]any{"releases": map[string]any{"bookworm": map[string]any{"status": "open", "fixed_version": "", "urgency": "high"}}}},
	}
	raw, err := json.Marshal(tracker)
	if err != nil {
		t.Fatal(err)
	}
	intel := &Intelligence{CacheDir: t.TempDir(), EnrichmentBudget: 50 * time.Millisecond}
	primeCache(t, intel, "https://security-tracker.debian.org/tracker/data/json", raw)
	blocked := httptest.NewServer(blockedHandler{})
	defer blocked.Close()
	intel.URLs = map[string]string{}
	for _, key := range []string{"osv-CVE-2024-10001", "github-CVE-2024-10001", "nvd-CVE-2024-10001", "cisa-kev", "epss"} {
		intel.URLs[key] = blocked.URL
	}
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "dpkg-query" {
			return []byte("ii \tssl\t1.0-1\tamd64\tssl\t1.0-1\t\t\t\n"), nil
		}
		return []byte("1.0"), nil
	})
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scan, err := ScanHost(context.Background(), system, store, "srv", nil, intel)
	if err != nil {
		t.Fatalf("an enrichment budget expiry must not fail the scan: %v", err)
	}
	if scan.Status != "completed" || scan.Coverage != "complete" {
		t.Fatalf("scan status %q coverage %q, want completed/complete", scan.Status, scan.Coverage)
	}
	if len(scan.Findings) != 1 || scan.Findings[0].Severity != "high" || scan.Findings[0].State != "confirmed" {
		t.Fatalf("distribution findings must stand unchanged: %+v", scan.Findings)
	}
	warned := false
	for _, source := range scan.Sources {
		if source.Name == "enrichment" && source.Error == "enrichment budget exceeded" {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("the budget expiry must be recorded as a warning source: %+v", scan.Sources)
	}
	found := false
	for _, warning := range scan.Warnings {
		if warning == "enrichment: enrichment budget exceeded" {
			found = true
		}
		if strings.Contains(warning, "deadline") || strings.Contains(warning, "context") {
			t.Fatalf("no DeadlineExceeded error may reach the warnings: %v", scan.Warnings)
		}
	}
	if !found {
		t.Fatalf("the enrichment warning must reach the scan warnings: %v", scan.Warnings)
	}
	if scan.Score.Value == nil || scan.Score.Status != "needs_attention" {
		t.Fatalf("the scan must still carry a score: %+v", scan.Score)
	}
}

// coverageFixtureTracker builds a Debian tracker body that marks the named
// sources resolved at the fixture install version (clean) and the named open
// sources affected without a fix.
func coverageFixtureTracker(t *testing.T, covered, open []string) []byte {
	t.Helper()
	tracker := map[string]any{}
	for _, name := range covered {
		tracker[name] = map[string]any{"CVE-2024-30000": map[string]any{"releases": map[string]any{"bookworm": map[string]any{"status": "resolved", "fixed_version": "1.0-1", "urgency": "low"}}}}
	}
	for _, name := range open {
		tracker[name] = map[string]any{"CVE-2024-30001": map[string]any{"releases": map[string]any{"bookworm": map[string]any{"status": "open", "fixed_version": "", "urgency": "high"}}}}
	}
	raw, err := json.Marshal(tracker)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// coverageFixturePackages names total fixture packages: the first named
// count get installed inventory entries.
func coverageFixtureNames(total int) []string {
	names := make([]string, 0, total)
	for i := 0; i < total; i++ {
		names = append(names, fmt.Sprintf("pkg%03d", i))
	}
	return names
}

func TestScanJSONKeepsEmptyCollectionsEmpty(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "dpkg-query" {
			return []byte("ii \tssl\t1.0-1\tamd64\tssl\t1.0-1\t\t\t\n"), nil
		}
		return []byte("1.0"), nil
	})
	inventoried, err := Collect(context.Background(), system, store, "srv", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(inventoried)
	if err != nil {
		t.Fatal(err)
	}
	for _, nullField := range []string{`"sources":null`, `"counts":null`, `"warnings":null`} {
		if strings.Contains(string(raw), nullField) {
			t.Fatalf("scan JSON must keep empty collections empty, got %s: %s", nullField, raw)
		}
	}

	// A match-path scan with nothing to warn about serializes warnings as
	// an empty array, matching the guide example.
	intel := &Intelligence{CacheDir: t.TempDir()}
	primeCache(t, intel, "https://security-tracker.debian.org/tracker/data/json", coverageFixtureTracker(t, []string{"ssl"}, nil))
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "dpkg-query" {
			return []byte("ii \tssl\t1.0-1\tamd64\tssl\t1.0-1\t\t\t\n"), nil
		}
		return []byte("1.0"), nil
	})
	matched, err := ScanHost(context.Background(), system, store, "srv", nil, intel)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(matched)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"warnings":[]`) {
		t.Fatalf("a quiet scan must serialize an empty warnings array: %s", raw)
	}
	for _, nullField := range []string{`"sources":null`, `"counts":null`, `"warnings":null`} {
		if strings.Contains(string(raw), nullField) {
			t.Fatalf("scan JSON must keep empty collections empty, got %s: %s", nullField, raw)
		}
	}
}

func TestScanHostUnknownPackagesWiring(t *testing.T) {
	names := coverageFixtureNames(50)
	intel := &Intelligence{CacheDir: t.TempDir()}
	primeCache(t, intel, "https://security-tracker.debian.org/tracker/data/json", coverageFixtureTracker(t, names[1:48], []string{"pkg000"}))
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "dpkg-query" {
			var b strings.Builder
			for _, pkg := range names {
				fmt.Fprintf(&b, "ii \t%s\t1.0-1\tamd64\t%s\t1.0-1\t\t\t\n", pkg, pkg)
			}
			return []byte(b.String()), nil
		}
		return []byte("1.0"), nil
	})
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scan, err := ScanHost(context.Background(), system, store, "srv", nil, intel)
	if err != nil {
		t.Fatal(err)
	}
	if scan.UnknownPackages != 2 {
		t.Fatalf("scan.UnknownPackages = %d, want 2", scan.UnknownPackages)
	}
	if scan.Coverage != "complete" || len(scan.Findings) != 1 || scan.Counts["high"] != 1 {
		t.Fatalf("unexpected scan: status %q coverage %q findings %d counts %v", scan.Status, scan.Coverage, len(scan.Findings), scan.Counts)
	}
	for _, warning := range scan.Warnings {
		if strings.Contains(warning, "coverage") {
			t.Fatalf("a scan below the degradation bounds must not warn: %v", scan.Warnings)
		}
	}
}

func TestScanHostUnknownPackagesWarningStaysInformational(t *testing.T) {
	names := coverageFixtureNames(5)
	intel := &Intelligence{CacheDir: t.TempDir()}
	primeCache(t, intel, "https://security-tracker.debian.org/tracker/data/json", coverageFixtureTracker(t, names[:3], nil))
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "dpkg-query" {
			var b strings.Builder
			for _, pkg := range names {
				fmt.Fprintf(&b, "ii \t%s\t1.0-1\tamd64\t%s\t1.0-1\t\t\t\n", pkg, pkg)
			}
			return []byte(b.String()), nil
		}
		return []byte("1.0"), nil
	})
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scan, err := ScanHost(context.Background(), system, store, "srv", nil, intel)
	if err != nil {
		t.Fatal(err)
	}
	if scan.UnknownPackages != 2 {
		t.Fatalf("scan.UnknownPackages = %d, want 2", scan.UnknownPackages)
	}
	// The unknown count is informational: it must never gate match
	// completeness, because advisory sources only list packages with
	// recorded issues and a realistic host is mostly absent from them.
	if scan.Coverage != "complete" || scan.Status != "completed" {
		t.Fatalf("an unknown count must not degrade coverage: status %q coverage %q", scan.Status, scan.Coverage)
	}
	named := false
	for _, source := range scan.Sources {
		if source.Name == "coverage" && strings.Contains(source.Error, "the count is informational") {
			named = true
		}
	}
	if !named {
		t.Fatalf("the unknown count must be named in the sources as informational: %+v", scan.Sources)
	}
	found := false
	for _, warning := range scan.Warnings {
		if strings.HasPrefix(warning, "coverage: ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unknown count must reach the scan warnings: %v", scan.Warnings)
	}
	// A realistic mixed fixture still carries a score value: stale-intel
	// and scan-age penalties apply, but the unknown count alone never
	// blanks the score.
	if scan.Score.Value == nil || scan.Score.Status == "unknown" {
		t.Fatalf("an informationally warned scan must still score: %+v", scan.Score)
	}
}

func TestMatchUnknownWarningBounds(t *testing.T) {
	cases := []struct {
		name    string
		total   int
		absent  int
		warning bool
	}{
		{name: "below both bounds", total: 50, absent: 2, warning: false},
		{name: "over the ratio bound", total: 5, absent: 2, warning: true},
		{name: "over the absolute bound", total: 1021, absent: 51, warning: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			names := coverageFixtureNames(tc.total)
			covered := names[:tc.total-tc.absent]
			intel := &Intelligence{CacheDir: t.TempDir()}
			primeCache(t, intel, "https://security-tracker.debian.org/tracker/data/json", coverageFixtureTracker(t, covered, nil))
			packages := []Package{}
			for _, name := range names {
				packages = append(packages, Package{Key: name + ":amd64@1.0-1", Name: name, Version: "1.0-1", SourceVersion: "1.0-1", Architecture: "amd64", Source: name})
			}
			_, sources, complete, unknown, err := intel.Match(context.Background(), System{}, Host{ID: "debian", Version: "12", Status: "supported"}, packages)
			if err != nil {
				t.Fatal(err)
			}
			if unknown != tc.absent {
				t.Fatalf("unknown = %d, want %d", unknown, tc.absent)
			}
			// The warning threshold never changes match completeness.
			if !complete {
				t.Fatal("an unknown count above the warning bounds must stay complete")
			}
			warned := false
			for _, source := range sources {
				if source.Name == "coverage" {
					warned = true
				}
			}
			if warned != tc.warning {
				t.Fatalf("coverage warning = %v, want %v", warned, tc.warning)
			}
		})
	}
	if coverageDegraded(0, 0) {
		t.Fatal("an empty inventory is not warned")
	}
	if !coverageDegraded(1021, 51) || coverageDegraded(1021, 50) {
		t.Fatal("the absolute bound is exclusive at 50")
	}
	if !coverageDegraded(19, 1) || coverageDegraded(20, 1) {
		t.Fatal("the ratio bound is exclusive at 5 percent")
	}
}

// TestRPMAdvisoriesFindingPath parses one updateinfo.xml end to end: the
// security type filter, epoch handling, the .xml.gz variant, and the
// potential state that keeps RPM findings honest.
func TestRPMAdvisoriesFindingPath(t *testing.T) {
	root := t.TempDir()
	updateinfo := `<?xml version="1.0"?><updates>` +
		`<update type="security" id="FEDORA-2026-aaa"><severity>high</severity>` +
		`<references><reference type="bugzilla" id="CVE-2024-90001" href="http://insecure.example/cve"/><reference type="bugzilla" id="CVE-2024-90001" href="https://secure.example/cve"/></references>` +
		`<pkglist><collection>` +
		`<package name="openssl" version="3.0.7" release="28.el9" epoch="1" arch="x86_64"/>` +
		`<package name="patched" version="3.0.7" release="28.el9" epoch="0" arch="x86_64"/>` +
		`</collection></pkglist></update>` +
		`<update type="bugfix" id="FEDORA-2026-bbb"><severity>low</severity>` +
		`<references><reference type="bugzilla" id="CVE-2024-90002" href="https://secure.example/bug"/></references>` +
		`<pkglist><collection><package name="openssl" version="3.0.7" release="28.el9" epoch="0" arch="x86_64"/></collection></pkglist></update>` +
		`</updates>`
	dir := filepath.Join(root, "var/cache/dnf", "repo-main")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "updateinfo.xml"), []byte(updateinfo), 0600); err != nil {
		t.Fatal(err)
	}
	gzUpdate := `<?xml version="1.0"?><updates>` +
		`<update type="security" id="FEDORA-2026-ccc"><severity>moderate</severity>` +
		`<references><reference type="bugzilla" id="CVE-2024-90003" href="https://secure.example/gz"/></references>` +
		`<pkglist><collection><package name="gzpkg" version="2.0" release="1.fc40" epoch="0" arch="x86_64"/></collection></pkglist></update>` +
		`<update type="security" id="FEDORA-2026-ddd"><severity>important</severity>` +
		`<references><reference type="bugzilla" id="CVE-2024-90004" href="https://secure.example/imp"/></references>` +
		`<pkglist><collection><package name="imppkg" version="2.0" release="1.fc40" epoch="0" arch="x86_64"/></collection></pkglist></update>` +
		`<update type="security" id="FEDORA-2026-eee"><severity>unspecified</severity>` +
		`<references><reference type="bugzilla" id="CVE-2024-90005" href="https://secure.example/unk"/></references>` +
		`<pkglist><collection><package name="unkpkg" version="2.0" release="1.fc40" epoch="0" arch="x86_64"/></collection></pkglist></update>` +
		`</updates>`
	gzDir := filepath.Join(root, "var/cache/dnf", "repo-other")
	if err := os.MkdirAll(gzDir, 0700); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(gzUpdate)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gzDir, "updateinfo.xml.gz"), buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	system := System{Root: root, Runner: fakeRunner(func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("dnf version comparison must not need a runner")
	})}
	packages := []Package{
		{Key: "openssl:x86_64@1:3.0.7-27.el9", Name: "openssl", Version: "1:3.0.7-27.el9", Architecture: "x86_64"},
		{Key: "patched:x86_64@1:3.0.9-1.el9", Name: "patched", Version: "1:3.0.9-1.el9", Architecture: "x86_64"},
		{Key: "gzpkg:x86_64@1.0-1.fc40", Name: "gzpkg", Version: "1.0-1.fc40", Architecture: "x86_64"},
		{Key: "imppkg:x86_64@1.0-1.fc40", Name: "imppkg", Version: "1.0-1.fc40", Architecture: "x86_64"},
		{Key: "unkpkg:x86_64@1.0-1.fc40", Name: "unkpkg", Version: "1.0-1.fc40", Architecture: "x86_64"},
	}
	findings, sources, _, err := (&Intelligence{}).rpmAdvisories(context.Background(), system, Host{ID: "fedora", Version: "40", Manager: "dnf", Status: "supported"}, packages)
	if err == nil {
		t.Fatal("cached repository metadata must never claim complete coverage")
	}
	byPackage := map[string][]Finding{}
	for _, f := range findings {
		byPackage[f.Package] = append(byPackage[f.Package], f)
	}
	if got := byPackage["openssl"]; len(got) != 2 || got[0].CVE != "CVE-2024-90001" || got[0].FixedVersion != "1:3.0.7-28.el9" ||
		got[0].State != "potential" || got[0].Severity != "high" || got[0].AdvisoryID != "FEDORA-2026-aaa" || got[1].AdvisoryID != "FEDORA-2026-aaa" {
		t.Fatalf("the epoch-dominant security advisory must produce one potential finding per reference: %+v", got)
	}
	for _, ref := range byPackage["openssl"][0].References {
		if strings.Contains(ref, "insecure.example") {
			t.Fatalf("an insecure reference URL must be dropped: %v", byPackage["openssl"][0].References)
		}
	}
	if got := byPackage["patched"]; len(got) != 0 {
		t.Fatalf("a package at or above the fix must stay silent: %+v", got)
	}
	if got := byPackage["gzpkg"]; len(got) != 1 || got[0].CVE != "CVE-2024-90003" || got[0].FixedVersion != "2.0-1.fc40" {
		t.Fatalf("the gzipped updateinfo must be parsed: %+v", got)
	}
	if byPackage["gzpkg"][0].Severity != "medium" {
		t.Fatalf("the rpm moderate severity must land on the medium vocabulary: %+v", byPackage["gzpkg"][0])
	}
	if got := byPackage["imppkg"]; len(got) != 1 || got[0].Severity != "high" {
		t.Fatalf("the rpm important severity must land on the high vocabulary: %+v", got)
	}
	if got := byPackage["unkpkg"]; len(got) != 1 || got[0].Severity != "unknown" {
		t.Fatalf("an off-vocabulary rpm severity must stay unknown: %+v", got)
	}
	for _, f := range findings {
		if f.CVE == "CVE-2024-90002" {
			t.Fatal("a bugfix advisory must be filtered out")
		}
	}
	if len(sources) < 2 {
		t.Fatalf("each parsed updateinfo file is a source: %+v", sources)
	}
}

// seedStaleCache writes a cache entry whose retrieval time is past the
// freshness window so fetch must go to the network before falling back.
func seedStaleCache(t *testing.T, intel *Intelligence, address string, body []byte) {
	t.Helper()
	sum := sha256.Sum256([]byte(address))
	digest := sha256.Sum256(body)
	cached := struct {
		Source Source
		Body   []byte
	}{Source: Source{Name: "fixture", URL: address, RetrievedAt: time.Now().UTC().Add(-48 * time.Hour), Digest: hex.EncodeToString(digest[:])}, Body: body}
	raw, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intel.CacheDir, hex.EncodeToString(sum[:])+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFetchFallsBackToStaleCacheOnFetchFailure(t *testing.T) {
	intel := &Intelligence{CacheDir: t.TempDir()}
	address := "https://fixture.invalid/tracker"
	body := []byte(`{"cached":true}`)
	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()
	// The cache is keyed by the resolved address, so the stale entry must
	// be seeded under the URL the fetch will actually request.
	intel.URLs = map[string]string{"debian": notFound.URL}
	seedStaleCache(t, intel, notFound.URL, body)
	got, source, err := intel.fetch(context.Background(), "debian", address)
	if err != nil {
		t.Fatalf("a failed refresh must fall back to the verified cache: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("the cached body must be served: %s", got)
	}
	if !source.Stale || source.Error == "" {
		t.Fatalf("the fallback must be marked stale with the fetch error: %+v", source)
	}
}

func TestFetchRetries429WithRetryAfter(t *testing.T) {
	intel := &Intelligence{CacheDir: t.TempDir()}
	address := "https://fixture.invalid/epss"
	attempts := 0
	body := []byte(`{"data":[]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()
	intel.URLs = map[string]string{"epss": server.URL}
	start := time.Now()
	got, source, err := intel.fetch(context.Background(), "epss", address)
	if err != nil {
		t.Fatalf("a 429 with Retry-After must be retried: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("the retried body must be served: %s", got)
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond {
		t.Fatalf("the Retry-After hint must be honored before the retry: %v", elapsed)
	}
	if source.Digest == "" || source.RetrievedAt.IsZero() {
		t.Fatalf("the successful refresh must carry provenance: %+v", source)
	}
}
