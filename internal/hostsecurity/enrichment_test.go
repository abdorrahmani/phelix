package hostsecurity

import (
	"context"
	"testing"

	"github.com/abdorrahmani/phelix/internal/securityscore"
)

func TestEnrichGitHubSeverityVocabulary(t *testing.T) {
	intel := &Intelligence{CacheDir: t.TempDir(), Offline: true}
	github := `[{"cve_id":"CVE-2024-40001","severity":"MODERATE","html_url":"https://github.com/advisories/GHSA-1"},{"cve_id":"CVE-2024-40002","severity":"Low","html_url":"https://github.com/advisories/GHSA-2"},{"cve_id":"CVE-2024-40003","severity":"CRITICAL","html_url":"https://github.com/advisories/GHSA-3"},{"cve_id":"CVE-2024-40004","severity":"catastrophic","html_url":"https://github.com/advisories/GHSA-4"}]`
	for _, cve := range []string{"CVE-2024-40001", "CVE-2024-40002", "CVE-2024-40003", "CVE-2024-40004"} {
		primeCache(t, intel, "https://api.github.com/advisories?cve_id="+cve, []byte(github))
	}
	findings := []Finding{
		{CVE: "CVE-2024-40001", Severity: "unknown", Risk: securityscore.Risk{Severity: "unknown"}},
		{CVE: "CVE-2024-40002", Severity: "unknown", Risk: securityscore.Risk{Severity: "unknown"}},
		{CVE: "CVE-2024-40003", Severity: "unknown", Risk: securityscore.Risk{Severity: "unknown"}},
		{CVE: "CVE-2024-40004", Severity: "unknown", Risk: securityscore.Risk{Severity: "unknown"}},
		{CVE: "CVE-2024-40005", Severity: "unknown", Risk: securityscore.Risk{Severity: "unknown"}},
	}
	intel.enrich(context.Background(), findings)
	want := []string{"medium", "low", "critical", "unknown", "unknown"}
	for i, f := range findings {
		if f.Severity != want[i] {
			t.Fatalf("%s severity = %q, want %q", f.CVE, f.Severity, want[i])
		}
		if f.Risk.Severity != f.Severity {
			t.Fatalf("%s risk severity = %q, want %q", f.CVE, f.Risk.Severity, f.Severity)
		}
	}
}

func TestEnrichGitHubSeverityKeepsDistributionSeverity(t *testing.T) {
	intel := &Intelligence{CacheDir: t.TempDir(), Offline: true}
	github := `[{"cve_id":"CVE-2024-40001","severity":"MODERATE","html_url":"https://github.com/advisories/GHSA-1"}]`
	primeCache(t, intel, "https://api.github.com/advisories?cve_id=CVE-2024-40001", []byte(github))
	findings := []Finding{{CVE: "CVE-2024-40001", Severity: "high"}}
	intel.enrich(context.Background(), findings)
	if findings[0].Severity != "high" {
		t.Fatalf("a distribution severity must not be overwritten: %q", findings[0].Severity)
	}
}

func TestNormalizeGitHubSeverity(t *testing.T) {
	for value, want := range map[string]string{"LOW": "low", "Moderate": "medium", "HIGH": "high", "critical": "critical", "": "unknown", "severe": "unknown"} {
		if got := normalizeGitHubSeverity(value); got != want {
			t.Fatalf("normalizeGitHubSeverity(%q) = %q, want %q", value, got, want)
		}
	}
}
