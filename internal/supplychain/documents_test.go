package supplychain

import (
	"context"
	"encoding/json"
	"github.com/abdorrahmani/phelix/internal/security"
	provenance "github.com/in-toto/attestation/go/predicates/provenance/v1"
	attestation "github.com/in-toto/attestation/go/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func populatedReport() *Report {
	r := fixtureReport()
	r.Source.SHA256 = Digest([]byte("source inputs"))
	r.Builder = Builder{ID: "https://github.com/abdorrahmani/phelix", PhelixVersion: "test", Language: "go", Compiler: "go", CompilerVersion: "go1.27.4"}
	r.Inventory.Roots = []string{"root"}
	r.Inventory.Modules = []security.Module{{Name: "example.org/direct", Key: "example.org/direct@v1.0.0", Version: "v1.0.0", Direct: true, Resolved: true, Confidence: security.ConfidenceResolved}, {Name: "example.org/transitive", Key: "example.org/transitive@v2.0.0", Version: "v2.0.0", Resolved: true, Confidence: security.ConfidenceResolved}}
	r.Inventory.Edges = []security.GraphEdge{{From: "root", To: r.Inventory.Modules[0].Key}, {From: r.Inventory.Modules[0].Key, To: r.Inventory.Modules[1].Key}}
	r.Licenses = []LicenseFinding{{Package: r.Inventory.Modules[0].Name, Version: "v1.0.0", Expression: "MIT OR Apache-2.0", Status: "identified", Reason: "test license metadata"}, {Package: r.Inventory.Modules[1].Name, Version: "v2.0.0", Status: "unknown", Reason: "license not available"}}
	return r
}

func TestDocumentsBindVersionArtifactAndInventory(t *testing.T) {
	s := NewStore(t.TempDir())
	r := populatedReport()
	if err := GenerateDocuments(s, r); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDocuments(s, r); err != nil {
		t.Fatal(err)
	}
	duplicate := *r
	duplicate.SBOMs = []Document{r.SBOMs[0], r.SBOMs[0]}
	if err := ValidateReport(&duplicate); err == nil {
		t.Fatal("duplicate SPDX documents substituted for CycloneDX")
	}
	original := r.Provenance.SHA256
	r.Version++
	if err := VerifyDocuments(s, r); err == nil {
		t.Fatal("different version accepted old provenance")
	}
	if err := GenerateDocuments(s, r); err != nil {
		t.Fatal(err)
	}
	if r.Provenance.SHA256 == original {
		t.Fatal("version change did not change provenance")
	}
	r.Inventory.Modules[0].Version = "v99.0.0"
	if err := VerifyDocuments(s, r); err == nil {
		t.Fatal("changed dependency inventory accepted")
	}
}

func TestTamperedArtifactBlockedInEveryPolicyMode(t *testing.T) {
	s := NewStore(t.TempDir())
	r := populatedReport()
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, []byte("artifact"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := GenerateDocuments(s, r); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"disabled", "audit", "enforce"} {
		p := DefaultPolicy()
		p.Mode = mode
		if err := s.SavePolicy(r.AppID, p); err != nil {
			t.Fatal(err)
		}
		if err := Verify(context.Background(), s, r, path); err == nil {
			t.Fatalf("tamper accepted in %s", mode)
		}
	}
}

func TestLicenseExpressionsAndMissingMetadata(t *testing.T) {
	r := populatedReport()
	p := DefaultPolicy()
	p.Mode = "enforce"
	p.RequireSBOM = false
	p.RequireProvenance = false
	p.AllowedLicenses = []string{"MIT"}
	if _, err := Evaluate(r, p); err != nil {
		t.Fatalf("approved OR alternative and unknown license should not be confirmed violations: %v", err)
	}
	r.Licenses[0].Expression = "MIT OR GPL-3.0-only"
	p.DeniedLicenses = []string{"GPL-3.0-only"}
	if decisions, err := Evaluate(r, p); err != nil {
		t.Fatalf("approved OR alternative incorrectly denied: %v %+v", err, decisions)
	}
	r.Licenses[0].Expression = "MIT AND GPL-3.0-only"
	p.DeniedLicenses = []string{"GPL-3.0-only"}
	if _, err := Evaluate(r, p); err == nil {
		t.Fatal("denied license hidden in expression accepted")
	}
	p.DeniedLicenses = []string{"NotARealLicense"}
	if err := ValidatePolicy(p); err == nil {
		t.Fatal("unknown SPDX policy accepted")
	}
}

func TestBuildInputMutationAndArguments(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	facts, err := Capture(context.Background(), BuildFacts{AppID: "app_test", AppName: "example", Directory: root, Language: "go", OutputPath: filepath.Join(root, "binary")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package main\nfunc main() {panic(1)}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(context.Background(), NewStore(t.TempDir()), facts, facts.OutputPath); err == nil {
		t.Fatal("source mutation accepted")
	}
	args := SafeArguments([]string{"-ldflags", "-X password=secret", "--token=secret", "--release"})
	raw, _ := json.Marshal(args)
	if string(raw) == "" || contains(args, "secret") || string(raw) == `["-ldflags","-X password=secret","--token=secret","--release"]` {
		t.Fatal("sensitive argument values retained")
	}
	args = SafeArguments([]string{"-ldflags -X secret", "--credential\tsecret", "--release"})
	if len(args) != 1 || args[0] != "--release" {
		t.Fatalf("compound argument exposed a value: %q", args)
	}
}

func TestOfficialStandardsValidation(t *testing.T) {
	if os.Getenv("PHELIX_VALIDATE_STANDARDS") != "1" {
		t.Skip("set PHELIX_VALIDATE_STANDARDS=1 to validate with official JSON schemas")
	}
	dir := t.TempDir()
	s := NewStore(t.TempDir())
	r := populatedReport()
	if err := GenerateDocuments(s, r); err != nil {
		t.Fatal(err)
	}
	for _, d := range append(r.SBOMs, r.Provenance) {
		raw, err := s.Document(d.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, d.Format+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := `import json, sys, urllib.request
from jsonschema import Draft7Validator, RefResolver
root=sys.argv[1]
base='https://raw.githubusercontent.com/CycloneDX/specification/1.6/schema/'
def load(url):
 with urllib.request.urlopen(url,timeout=30) as response: return json.load(response)
spdx=load('https://raw.githubusercontent.com/spdx/spdx-spec/v2.3/schemas/spdx-schema.json')
Draft7Validator(spdx).validate(json.load(open(root+'/spdx-2.3.json')))
cyclone=load(base+'bom-1.6.schema.json')
resolver=RefResolver(base_uri=base+'bom-1.6.schema.json',referrer=cyclone,store={base+'spdx.schema.json':load(base+'spdx.schema.json'),base+'jsf-0.82.schema.json':load(base+'jsf-0.82.schema.json')})
Draft7Validator(cyclone,resolver=resolver).validate(json.load(open(root+'/cyclonedx-1.6.json')))
statement=json.load(open(root+'/slsa-provenance-v1.json'))
assert statement['_type']=='https://in-toto.io/Statement/v1'
assert statement['predicateType']=='https://slsa.dev/provenance/v1'
print('Official SPDX 2.3 and CycloneDX 1.6 JSON schemas passed')`
	cmd := exec.Command("python3", "-c", script, dir)
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("official validation failed: %v\n%s", err, raw)
	} else {
		t.Log(string(raw))
	}
}

func TestOfficialSLSAProvenance(t *testing.T) {
	raw, err := Provenance(populatedReport())
	if err != nil {
		t.Fatal(err)
	}
	var statement attestation.Statement
	if err := protojson.Unmarshal(raw, &statement); err != nil {
		t.Fatal(err)
	}
	if statement.Type != "https://in-toto.io/Statement/v1" || statement.PredicateType != "https://slsa.dev/provenance/v1" || len(statement.Subject) != 1 {
		t.Fatal("invalid in-toto statement")
	}
	predicateJSON, err := protojson.Marshal(statement.Predicate)
	if err != nil {
		t.Fatal(err)
	}
	var predicate provenance.Provenance
	if err = protojson.Unmarshal(predicateJSON, &predicate); err != nil {
		t.Fatal(err)
	}
	if predicate.BuildDefinition.BuildType != "https://phelix.dev/build/native/v1" || predicate.RunDetails.Builder.Id == "" {
		t.Fatal("missing build provenance fields")
	}
}

func TestNativeBuildDefersRequiredScanUntilFinalization(t *testing.T) {
	t.Setenv("PHELIX_SIGNING_KEY", "")
	t.Setenv("PHELIX_KEYLESS_SIGNING", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "binary")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s := NewStore(t.TempDir())
	p := DefaultPolicy()
	p.Mode = "enforce"
	p.RequireScan = true
	if err := s.SavePolicy("app_test", p); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	facts, err := Capture(ctx, BuildFacts{AppID: "app_test", AppName: "example", Directory: dir, Language: "go", OutputPath: path})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "go", "build", "-o", path, "main.go")
	command.Dir = dir
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile test artifact: %v %s", err, raw)
	}
	r, err := Generate(ctx, s, facts, path)
	if err != nil {
		t.Fatalf("artifact generation blocked a scan that runs during finalization: %v", err)
	}
	final, err := Finalize(ctx, s, r, path, 1)
	if err != nil || final.Scan == nil || final.Scan.Status != security.ScanCompleted {
		t.Fatalf("finalization did not fulfill required scan: %+v %v", final, err)
	}
}
