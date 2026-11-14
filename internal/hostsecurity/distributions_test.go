package hostsecurity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestScanHostUnknownPackagesWiring(t *testing.T) {
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
	enrichment := httptest.NewServer(unavailableHandler{})
	defer enrichment.Close()
	intel.URLs = map[string]string{}
	for _, key := range []string{"osv-CVE-2024-10001", "github-CVE-2024-10001", "nvd-CVE-2024-10001", "cisa-kev", "epss"} {
		intel.URLs[key] = enrichment.URL
	}
	system := fixtureSystem(t, "debian")
	system.Runner = fakeRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "dpkg-query" {
			return []byte("ii \tssl\t1.0-1\tamd64\tssl\t1.0-1\t\t\t\nii \tgone\t2.0-1\tamd64\tgone\t2.0-1\t\t\t\nii \tphantom\t3.0-1\tamd64\tphantom\t3.0-1\t\t\t\n"), nil
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
}
