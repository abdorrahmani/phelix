package supplychain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/security"
)

func TestApplicationVerificationLogsChangedAuditPolicy(t *testing.T) {
	s := NewStore(t.TempDir())
	t.Cleanup(logs.ReopenFileLog)
	t.Setenv("PHELIX_DATA_DIR", s.DataDir)
	logs.ReopenFileLog()
	r := populatedReport()
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, []byte("artifact"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := GenerateDocuments(s, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := SaveSidecar(path, r); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(s.root(), "reports", r.AppID, Reference(r)+".json")
	before, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy()
	p.RequireSignature = true
	p.TrustedIdentity = "builder@example.com"
	p.TrustedIssuer = "https://issuer.example.com"
	p.DeniedLicenses = []string{"MIT", "Apache-2.0"}
	if err := s.SavePolicy(r.AppID, p); err != nil {
		t.Fatal(err)
	}
	if err := VerifyApplicationPath(context.Background(), s, path, r.AppID, r.AppName); err != nil {
		t.Fatalf("audit policy blocked artifact launch: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(s.DataDir, "logs", "phelix.log"))
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var record logs.LogRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Component != "security" || record.Level != logs.LevelWarning || !strings.Contains(record.Msg, "app="+r.AppID) || !strings.Contains(record.Msg, "artifact="+Reference(r)) {
			continue
		}
		for _, rule := range []string{"signature", "license"} {
			if strings.Contains(record.Msg, "rule="+rule+" ") && strings.Contains(record.Msg, "reason=") {
				rules[rule] = true
			}
		}
	}
	if !rules["signature"] || !rules["license"] {
		t.Fatalf("current audit violations were discarded: %s", raw)
	}
	after, err := os.ReadFile(reportPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("read-only launch verification rewrote historical evidence: %v", err)
	}
	p.Mode = "enforce"
	if err := s.SavePolicy(r.AppID, p); err != nil {
		t.Fatal(err)
	}
	if err := VerifyApplicationPath(context.Background(), s, path, r.AppID, r.AppName); err == nil {
		t.Fatal("enforcing current policy accepted an unsigned denied artifact")
	}
}

func TestEnforcedPolicyErrorKeepsDetailsLocal(t *testing.T) {
	s := NewStore(t.TempDir())
	t.Cleanup(logs.ReopenFileLog)
	t.Setenv("PHELIX_DATA_DIR", s.DataDir)
	logs.ReopenFileLog()
	r := populatedReport()
	r.Scan = &security.ScanRecord{Findings: []security.Finding{{AdvisoryID: "CVE-2026-1234", Package: "example.org/private", Version: "v1.0.0", State: security.StateAffected, Severity: security.SeverityCritical}}}
	r.Licenses = []LicenseFinding{{Package: "example.org/private", Expression: "GPL-3.0-only", Status: "identified"}}
	p := DefaultPolicy()
	p.Mode = "enforce"
	p.RequireSBOM = false
	p.RequireProvenance = false
	p.DeniedLicenses = []string{"GPL-3.0-only"}
	decisions, err := Evaluate(r, p)
	if phelixerr.CodeOf(err) != phelixerr.CodeSecurityPolicyRejected {
		t.Fatalf("enforcement did not block the artifact: %v", err)
	}
	for _, private := range []string{"CVE-2026-1234", "example.org/private", "GPL-3.0-only"} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("ordinary error exposes security detail %q: %v", private, err)
		}
	}
	if cause := phelixerr.Cause(err); cause == nil || !strings.Contains(cause.Error(), "CVE-2026-1234") || !strings.Contains(cause.Error(), "example.org/private") {
		t.Fatalf("local diagnostic cause lost the rejection reason: %v", cause)
	}
	raw, readErr := os.ReadFile(filepath.Join(s.DataDir, "logs", "phelix.log"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, rule := range []string{"vulnerability", "license"} {
		found := false
		for _, decision := range decisions {
			if decision.Rule == rule && decision.Status == "violation" && strings.Contains(decision.Reason, "example.org/private") {
				for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
					var record logs.LogRecord
					if err := json.Unmarshal([]byte(line), &record); err != nil {
						t.Fatal(err)
					}
					if record.Component == "security" && record.Level == logs.LevelWarning && strings.Contains(record.Msg, "app="+r.AppID) && strings.Contains(record.Msg, "artifact="+Reference(r)) && strings.Contains(record.Msg, "rule="+rule+" reason="+decision.Reason) {
						found = true
					}
				}
			}
		}
		if !found {
			t.Fatalf("local enforcement detail lost for %s: decisions=%v logs=%s", rule, decisions, raw)
		}
	}
}
