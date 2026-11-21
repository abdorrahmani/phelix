package supplychain

import (
	"errors"
	"fmt"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/github/go-spdx/v2/spdxexp"
	"strings"
	"time"
)

func ValidatePolicy(p Policy) error {
	if p.Mode != "disabled" && p.Mode != "audit" && p.Mode != "enforce" {
		return errors.New("policy mode must be disabled, audit or enforce")
	}
	if security.SeverityRank(p.Severity) == 0 {
		return errors.New("policy severity must be low, medium, high or critical")
	}
	if (p.TrustedIdentity == "") != (p.TrustedIssuer == "") {
		return errors.New("trusted signing identity and issuer must be configured together")
	}
	if strings.ContainsAny(p.TrustedIdentity+p.TrustedIssuer, "\x00\n\r") || len(p.TrustedIdentity) > 1024 || len(p.TrustedIssuer) > 1024 {
		return errors.New("invalid signing trust identity")
	}
	if p.PublicKey != "" && (len(p.PublicKey) > 4096 || strings.ContainsAny(p.PublicKey, "\x00\n\r") || !strings.HasSuffix(p.PublicKey, ".pub") || strings.Contains(p.PublicKey, "://") || strings.HasPrefix(p.PublicKey, "-")) {
		return errors.New("verification requires a local .pub public key")
	}
	if p.RequireSignature && p.PublicKey == "" && p.TrustedIdentity == "" {
		return errors.New("mandatory signatures require a trusted public key or identity and issuer")
	}
	if len(p.AllowedLicenses)+len(p.DeniedLicenses) > 256 {
		return errors.New("too many license policy entries")
	}
	for _, v := range append(append([]string{}, p.AllowedLicenses...), p.DeniedLicenses...) {
		if v == "" || len(v) > 256 || strings.ContainsAny(v, "\x00\r\n") {
			return errors.New("invalid license policy entry")
		}
	}
	if valid, _ := spdxexp.ValidateLicensesWithOptions(append(append([]string{}, p.AllowedLicenses...), p.DeniedLicenses...), spdxexp.ValidateLicensesOptions{FailComplexExpressions: true}); !valid {
		return errors.New("license policies require SPDX identifiers or identifiers with exceptions")
	}
	return nil
}

func Evaluate(r *Report, p Policy) ([]Decision, error) {
	if err := ValidatePolicy(p); err != nil {
		return nil, err
	}
	var ds []Decision
	add := func(rule string, ok bool, reason string) {
		status := "pass"
		if !ok {
			status = "violation"
		}
		ds = append(ds, Decision{Rule: rule, Status: status, Reason: reason})
	}
	add("sbom", !p.RequireSBOM || len(r.SBOMs) == 2, "both SPDX and CycloneDX documents are required")
	add("provenance", !p.RequireProvenance || r.Provenance.SHA256 != "", "build provenance is required")
	add("signature", !p.RequireSignature || r.Signature.Status == "verified", "a freshly verified signature from a configured trust root is required")
	complete := r.Scan != nil && r.Scan.Status == security.ScanCompleted && r.Scan.Coverage == security.CoverageComplete && r.Scan.Freshness == security.FreshnessFresh && r.Scan.FinishedAt != nil && time.Since(*r.Scan.FinishedAt) <= 24*time.Hour && r.Scan.FinishedAt.Before(time.Now().Add(5*time.Minute)) && r.Scan.Counts.Unknown == 0
	add("scan", !p.RequireScan || complete, "a complete fresh vulnerability scan is required")
	if r.Inventory.Level != security.InventoryResolved {
		add("inventory", false, "the complete resolved dependency inventory is unavailable")
	}
	if r.Scan != nil {
		for _, f := range r.Scan.Findings {
			if f.State == security.StateAffected || f.State == security.StatePotentiallyAffected {
				if security.SeverityRank(f.Severity) >= security.SeverityRank(p.Severity) || f.Severity == security.SeverityUnknown {
					add("vulnerability", false, fmt.Sprintf("%s affects %s@%s (%s)", f.AdvisoryID, f.Package, f.Version, f.Severity))
				}
			}
		}
	}
	for _, l := range r.Licenses {
		if l.Expression == "" || l.Status == "unknown" {
			if p.WarnUnknownLicenses {
				ds = append(ds, Decision{Rule: "license", Status: "review", Reason: l.Package + " requires manual license review"})
			}
			continue
		}
		ids, err := spdxexp.ExtractLicenses(l.Expression)
		if err != nil {
			ds = append(ds, Decision{Rule: "license", Status: "review", Reason: l.Package + " has an invalid license expression"})
			continue
		}
		candidates := []string{}
		deniedFound := false
		for _, id := range ids {
			if contains(p.DeniedLicenses, id) || contains(p.DeniedLicenses, strings.SplitN(id, " WITH ", 2)[0]) {
				deniedFound = true
				continue
			}
			candidates = append(candidates, id)
		}
		if len(p.AllowedLicenses) > 0 {
			candidates = nil
			for _, id := range p.AllowedLicenses {
				if !contains(p.DeniedLicenses, id) && !contains(p.DeniedLicenses, strings.SplitN(id, " WITH ", 2)[0]) {
					candidates = append(candidates, id)
				}
			}
		}
		allowed := false
		if len(candidates) > 0 {
			allowed, err = spdxexp.Satisfies(l.Expression, candidates)
			if err != nil {
				return ds, err
			}
		}
		if !allowed {
			add("license", false, l.Package+" has no license alternative permitted by policy: "+l.Expression)
		} else if deniedFound {
			ds = append(ds, Decision{Rule: "license", Status: "review", Reason: l.Package + " requires selection of an approved license alternative: " + l.Expression})
		}

	}
	if p.Mode == "enforce" {
		for _, d := range ds {
			if d.Status == "violation" {
				return ds, errors.New("security policy rejected artifact: " + d.Reason)
			}
		}
	}
	return ds, nil
}

func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}
