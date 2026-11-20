package supplychain

import (
	"github.com/abdorrahmani/phelix/internal/security"
	"time"
)

const MaxDocumentBytes = 16 << 20
const MaxReportBytes = 32 << 20

type Artifact struct {
	Filename   string `json:"filename"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size_bytes"`
	TargetOS   string `json:"target_os,omitempty"`
	TargetArch string `json:"target_arch,omitempty"`
}

type Document struct {
	Format    string `json:"format"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type Source struct {
	Repository string `json:"repository,omitempty"`
	Commit     string `json:"commit,omitempty"`
	Ref        string `json:"ref,omitempty"`
	Dirty      bool   `json:"dirty"`
}

type Builder struct {
	ID              string   `json:"id"`
	PhelixVersion   string   `json:"phelix_version"`
	Language        string   `json:"language"`
	Compiler        string   `json:"compiler"`
	CompilerVersion string   `json:"compiler_version,omitempty"`
	Arguments       []string `json:"arguments,omitempty"`
}

type LicenseFinding struct {
	Package    string `json:"package"`
	Version    string `json:"version,omitempty"`
	Expression string `json:"expression,omitempty"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
}

type Signature struct {
	Status         string     `json:"status"`
	Identity       string     `json:"identity,omitempty"`
	Issuer         string     `json:"issuer,omitempty"`
	KeyFingerprint string     `json:"key_fingerprint,omitempty"`
	Bundles        []Document `json:"bundles,omitempty"`
	Reason         string     `json:"reason,omitempty"`
}

type Policy struct {
	Mode                string   `json:"mode"`
	Severity            string   `json:"severity"`
	AllowedLicenses     []string `json:"allowed_licenses,omitempty"`
	DeniedLicenses      []string `json:"denied_licenses,omitempty"`
	WarnUnknownLicenses bool     `json:"warn_unknown_licenses"`
	RequireSBOM         bool     `json:"require_sbom"`
	RequireProvenance   bool     `json:"require_provenance"`
	RequireSignature    bool     `json:"require_signature"`
	RequireScan         bool     `json:"require_scan"`
	TrustedIdentity     string   `json:"trusted_identity,omitempty"`
	TrustedIssuer       string   `json:"trusted_issuer,omitempty"`
	PublicKey           string   `json:"public_key,omitempty"`
}

type Decision struct {
	Rule   string `json:"rule"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type Report struct {
	SchemaVersion int                  `json:"schema_version"`
	AppID         string               `json:"app_id"`
	AppName       string               `json:"app_name"`
	Version       int                  `json:"version"`
	BuiltAt       time.Time            `json:"built_at"`
	Artifact      Artifact             `json:"artifact"`
	Source        Source               `json:"source"`
	Builder       Builder              `json:"builder"`
	Inventory     *security.Inventory  `json:"inventory"`
	Licenses      []LicenseFinding     `json:"licenses"`
	Scan          *security.ScanRecord `json:"scan,omitempty"`
	SBOMs         []Document           `json:"sboms"`
	Provenance    Document             `json:"provenance"`
	Signature     Signature            `json:"signature"`
	Policy        Policy               `json:"policy"`
	Decisions     []Decision           `json:"decisions"`
	Documents     map[string][]byte    `json:"documents,omitempty"`
}

func DefaultPolicy() Policy {
	return Policy{Mode: "audit", Severity: "high", WarnUnknownLicenses: true, RequireSBOM: true, RequireProvenance: true}
}
