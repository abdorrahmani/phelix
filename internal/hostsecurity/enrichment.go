package hostsecurity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/security"
)

func (i *Intelligence) enrich(ctx context.Context, findings []Finding) []Source {
	sources := []Source{}
	indices := map[string][]int{}
	ordered := []string{}
	for n, f := range findings {
		if _, ok := indices[f.CVE]; !ok {
			ordered = append(ordered, f.CVE)
		}
		indices[f.CVE] = append(indices[f.CVE], n)
	}
	if len(ordered) == 0 {
		return sources
	}
	for n, cve := range ordered {
		if n >= 25 || ctx.Err() != nil {
			break
		}
		for _, provider := range []string{"osv", "github", "nvd"} {
			address := map[string]string{"osv": "https://api.osv.dev/v1/vulns/" + url.PathEscape(cve), "github": "https://api.github.com/advisories?cve_id=" + url.QueryEscape(cve), "nvd": "https://services.nvd.nist.gov/rest/json/cves/2.0?cveId=" + url.QueryEscape(cve)}[provider]
			raw, source, err := i.fetch(ctx, provider+"-"+cve, address)
			sources = append(sources, source)
			if err != nil {
				continue
			}
			for _, idx := range indices[cve] {
				f := &findings[idx]
				switch provider {
				case "osv":
					var adv security.OSVAdvisory
					if json.Unmarshal(raw, &adv) != nil {
						continue
					}
					if adv.ID != cve && !contains(adv.Aliases, cve) {
						continue
					}
					for _, ref := range adv.References {
						if link := safeURL(ref.URL); link != "" {
							f.References = append(f.References, link)
						}
					}
					f.Published = adv.Published
					f.Modified = adv.Modified
				case "github":
					var records []struct {
						CVE      string `json:"cve_id"`
						URL      string `json:"html_url"`
						Severity string `json:"severity"`
					}
					if json.Unmarshal(raw, &records) != nil {
						continue
					}
					for _, r := range records {
						if r.CVE != cve {
							continue
						}
						if link := safeURL(r.URL); link != "" {
							f.References = append(f.References, link)
						}
						if f.Severity == "unknown" {
							f.Severity = r.Severity
						}
					}
				case "nvd":
					var response struct {
						Vulnerabilities []struct {
							CVE struct {
								ID        string `json:"id"`
								Published string `json:"published"`
								Modified  string `json:"lastModified"`
								Metrics   map[string][]struct {
									Data struct {
										Score    float64 `json:"baseScore"`
										Severity string  `json:"baseSeverity"`
									} `json:"cvssData"`
								} `json:"metrics"`
							} `json:"cve"`
						} `json:"vulnerabilities"`
					}
					if json.Unmarshal(raw, &response) != nil {
						continue
					}
					for _, record := range response.Vulnerabilities {
						if record.CVE.ID != cve {
							continue
						}
						f.Published = record.CVE.Published
						f.Modified = record.CVE.Modified
						for _, key := range []string{"cvssMetricV40", "cvssMetricV31", "cvssMetricV30", "cvssMetricV2"} {
							metrics := record.CVE.Metrics[key]
							if len(metrics) == 0 {
								continue
							}
							score := metrics[0].Data.Score
							if score < 0 || score > 10 {
								continue
							}
							f.Risk.CVSS = &score
							if f.Severity == "unknown" {
								f.Severity = strings.ToLower(metrics[0].Data.Severity)
							}
							break
						}
					}
				}
				f.Sources = append(f.Sources, source)
				f.Risk.Severity = f.Severity
			}
		}
		if n+1 < len(ordered) && !i.Offline {
			if err := waitContext(ctx, 6*time.Second); err != nil {
				break
			}
		}
	}
	if raw, source, err := i.fetch(ctx, "cisa-kev", "https://www.cisa.gov/sites/default/files/feeds/known_exploited_vulnerabilities.json"); err == nil {
		var catalog struct {
			Vulnerabilities []struct {
				CVE string `json:"cveID"`
			} `json:"vulnerabilities"`
		}
		if json.Unmarshal(raw, &catalog) == nil {
			known := map[string]bool{}
			for _, v := range catalog.Vulnerabilities {
				known[v.CVE] = true
			}
			for n := range findings {
				value := known[findings[n].CVE]
				findings[n].Risk.KnownExploited = &value
				findings[n].Sources = append(findings[n].Sources, source)
			}
		}
		sources = append(sources, source)
	} else {
		sources = append(sources, source)
	}
	ids := ordered
	if len(ids) > 100 {
		ids = ids[:100]
	}
	address := "https://api.first.org/data/v1/epss?cve=" + url.QueryEscape(strings.Join(ids, ","))
	if raw, source, err := i.fetch(ctx, "epss", address); err == nil {
		var response struct {
			Data []struct {
				CVE  string      `json:"cve"`
				EPSS json.Number `json:"epss"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &response) == nil {
			for _, v := range response.Data {
				value, err := v.EPSS.Float64()
				if err != nil || value < 0 || value > 1 {
					continue
				}
				for _, idx := range indices[v.CVE] {
					findings[idx].Risk.EPSS = &value
					findings[idx].Sources = append(findings[idx].Sources, source)
				}
			}
		}
		sources = append(sources, source)
	} else {
		sources = append(sources, source)
	}
	if len(ordered) > 25 {
		sources = append(sources, Source{Name: "enrichment", Error: fmt.Sprintf("%d CVEs exceed the 25-CVE enrichment limit; their missing risk factors remain unknown", len(ordered)-25)})
	}
	return sources
}

func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
