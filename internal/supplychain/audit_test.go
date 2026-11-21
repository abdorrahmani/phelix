package supplychain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/logs"
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
