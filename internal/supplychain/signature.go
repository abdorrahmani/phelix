package supplychain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func Sign(ctx context.Context, s *Store, r *Report, path, keyRef string) error {
	return sign(ctx, s, r, path, keyRef, true)
}

func sign(ctx context.Context, s *Store, r *Report, path, keyRef string, persist bool) error {
	if _, err := exec.LookPath("cosign"); err != nil {
		r.Signature = Signature{Status: "unavailable", Reason: "cosign is not installed"}
		return errors.New(r.Signature.Reason)
	}
	if strings.HasPrefix(keyRef, "-") || strings.ContainsAny(keyRef, "\x00\n\r") {
		return errors.New("invalid signing key reference")
	}
	p, err := s.LoadPolicy(r.AppID)
	if err != nil {
		return err
	}
	if p.PublicKey == "" && p.TrustedIdentity == "" {
		return errors.New("configure a trusted verification public key or identity and issuer before signing")
	}
	if err = VerifyDocuments(s, r); err != nil {
		return err
	}
	if hash, size, err := SHA256File(path); err != nil || hash != r.Artifact.SHA256 || size != r.Artifact.SizeBytes {
		return errors.New("artifact changed before signing")
	}
	temp, err := os.MkdirTemp("", "phelix-sign-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	paths := []string{path}
	for _, d := range append(append([]Document{}, r.SBOMs...), r.Provenance) {
		raw, err := s.Document(d.SHA256)
		if err != nil {
			return err
		}
		file := filepath.Join(temp, d.SHA256+".json")
		if err = os.WriteFile(file, raw, 0600); err != nil {
			return err
		}
		paths = append(paths, file)
	}
	r.Signature = Signature{Status: "signed", Identity: p.TrustedIdentity, Issuer: p.TrustedIssuer}
	for i, blob := range paths {
		bundle := filepath.Join(temp, fmt.Sprintf("bundle-%d.json", i))
		args := []string{"sign-blob", "--yes", "--bundle", bundle}
		if keyRef != "" {
			args = append(args, "--key", keyRef)
		}
		args = append(args, blob)
		runCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		command := exec.CommandContext(runCtx, "cosign", args...)
		err = command.Run()
		cancel()
		if err != nil {
			r.Signature.Status = "failed"
			r.Signature.Reason = "cosign signing failed"
			return errors.New(r.Signature.Reason)
		}
		raw, err := readBounded(bundle, MaxDocumentBytes)
		if err != nil {
			return err
		}
		doc, err := s.PutDocument("cosign-bundle", raw)
		if err != nil {
			return err
		}
		r.Signature.Bundles = append(r.Signature.Bundles, doc)
	}
	if err = VerifySignature(ctx, s, r, path, p); err != nil {
		return err
	}
	if !persist {
		return nil
	}
	if err = SaveSidecar(path, r); err != nil {
		return err
	}
	return s.Save(r)
}

func VerifySignature(ctx context.Context, s *Store, r *Report, path string, p Policy) error {
	if err := ValidateReport(r); err != nil {
		return err
	}
	if err := ValidatePolicy(p); err != nil {
		return err
	}
	if len(r.Signature.Bundles) == 0 {
		r.Signature.Status = "missing"
		return errors.New("artifact signature is missing")
	}
	r.Signature.Status = "untrusted"
	if p.PublicKey == "" && p.TrustedIdentity == "" {
		return errors.New("no signing trust root is configured")
	}
	if _, err := exec.LookPath("cosign"); err != nil {
		r.Signature.Status = "unavailable"
		return errors.New("cosign verification is unavailable")
	}
	if len(r.Signature.Bundles) != len(r.SBOMs)+2 {
		r.Signature.Status = "invalid"
		return errors.New("artifact, SBOM and provenance signatures are required")
	}
	if err := VerifyDocuments(s, r); err != nil {
		r.Signature.Status = "invalid"
		return err
	}
	hash, size, err := SHA256File(path)
	if err != nil || hash != r.Artifact.SHA256 || size != r.Artifact.SizeBytes {
		r.Signature.Status = "invalid"
		return errors.New("signed artifact differs from its recorded digest")
	}
	temp, err := os.MkdirTemp("", "phelix-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	paths := []string{path}
	for _, d := range append(append([]Document{}, r.SBOMs...), r.Provenance) {
		raw, err := s.Document(d.SHA256)
		if err != nil {
			return err
		}
		file := filepath.Join(temp, d.SHA256+".json")
		if err = os.WriteFile(file, raw, 0600); err != nil {
			return err
		}
		paths = append(paths, file)
	}
	fingerprint := ""
	publicKey := ""
	if p.PublicKey != "" {
		raw, err := readBounded(p.PublicKey, 64<<10)
		if err != nil {
			return errors.New("trusted public key is unavailable")
		}
		fingerprint = Digest(raw)
		publicKey = filepath.Join(temp, "trusted.pub")
		if err = os.WriteFile(publicKey, raw, 0600); err != nil {
			return err
		}
	}
	for i, d := range r.Signature.Bundles {
		raw, err := s.Document(d.SHA256)
		if err != nil {
			return err
		}
		file := filepath.Join(temp, fmt.Sprintf("bundle-%d.json", i))
		if err = os.WriteFile(file, raw, 0600); err != nil {
			return err
		}
		args := []string{"verify-blob", "--bundle", file}
		if p.PublicKey != "" {
			args = append(args, "--key", publicKey)
		} else {
			args = append(args, "--certificate-identity", p.TrustedIdentity, "--certificate-oidc-issuer", p.TrustedIssuer)
		}
		args = append(args, paths[i])
		runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		command := exec.CommandContext(runCtx, "cosign", args...)
		err = command.Run()
		cancel()
		if err != nil {
			r.Signature.Status = "invalid"
			r.Signature.Reason = "signature or trusted signing identity verification failed"
			return errors.New(r.Signature.Reason)
		}
	}
	hash, size, err = SHA256File(path)
	if err != nil || hash != r.Artifact.SHA256 || size != r.Artifact.SizeBytes {
		r.Signature.Status = "invalid"
		return errors.New("artifact changed during signature verification")
	}
	r.Signature.Status = "verified"
	r.Signature.Identity = p.TrustedIdentity
	r.Signature.Issuer = p.TrustedIssuer
	r.Signature.KeyFingerprint = fingerprint
	r.Signature.Reason = "artifact, SBOMs and provenance independently verified with configured trust"
	return nil
}

func Verify(ctx context.Context, s *Store, r *Report, path string) error {
	if err := ValidateReport(r); err != nil {
		return err
	}
	latest, loadErr := s.Load(r.AppID, Reference(r))
	if loadErr == nil {
		if latest.Provenance.SHA256 != r.Provenance.SHA256 || len(latest.SBOMs) != len(r.SBOMs) {
			return errors.New("artifact security documents differ from recorded build")
		}
		for i := range r.SBOMs {
			if latest.SBOMs[i].SHA256 != r.SBOMs[i].SHA256 {
				return errors.New("artifact SBOM differs from recorded build")
			}
		}
		r.Scan = latest.Scan
		r.Signature = latest.Signature
	} else if !os.IsNotExist(loadErr) {
		return loadErr
	}
	hash, size, err := SHA256File(path)
	if err != nil {
		r.Decisions = append(r.Decisions, Decision{Rule: "artifact_integrity", Status: "violation", Reason: "artifact is missing or unreadable"})
		return fmt.Errorf("artifact integrity verification failed: %w", err)
	}
	if hash != r.Artifact.SHA256 || size != r.Artifact.SizeBytes {
		r.Decisions = append(r.Decisions, Decision{Rule: "artifact_integrity", Status: "violation", Reason: "artifact SHA-256 or size differs from build record"})
		return errors.New("artifact integrity verification failed: SHA-256 or size differs from build record")
	}
	if err = VerifyDocuments(s, r); err != nil {
		return err
	}
	p, err := s.LoadPolicy(r.AppID)
	if err != nil {
		return err
	}
	r.Policy = p
	if len(r.Signature.Bundles) > 0 {
		err = VerifySignature(ctx, s, r, path, p)
		if err != nil && p.RequireSignature && p.Mode == "enforce" {
			return err
		}
	} else {
		r.Signature.Status = "missing"
	}
	r.Decisions, err = Evaluate(r, p)
	r.Decisions = append(r.Decisions, Decision{Rule: "artifact_integrity", Status: "pass", Reason: "artifact SHA-256 and size verified from current bytes"})
	return err
}

func VerifyPath(ctx context.Context, s *Store, path, appID string) error {
	r, err := LoadSidecar(path)
	if os.IsNotExist(err) {
		relative, relErr := filepath.Rel(filepath.Join(s.DataDir, "apps"), path)
		parts := strings.Split(relative, string(filepath.Separator))
		if relErr == nil && len(parts) == 4 && parts[1] == "builds" && parts[3] == "binary" && strings.HasPrefix(parts[2], "v") {
			raw, readErr := readBounded(filepath.Join(s.DataDir, "apps", parts[0], "versions.json"), MaxReportBytes)
			if readErr != nil {
				return readErr
			}
			var index struct {
				Versions []struct {
					Version  int     `json:"version"`
					SHA256   string  `json:"sha256"`
					Security *Report `json:"security"`
				} `json:"versions"`
			}
			if err = json.Unmarshal(raw, &index); err != nil {
				return err
			}
			number, parseErr := strconv.Atoi(strings.TrimPrefix(parts[2], "v"))
			if parseErr != nil {
				return parseErr
			}
			for _, v := range index.Versions {
				if v.Version == number {
					if v.Security != nil {
						copy := *v.Security
						return Verify(ctx, s, &copy, path)
					}
					if v.SHA256 != "" {
						hash, _, err := SHA256File(path)
						if err != nil {
							return err
						}
						if hash != v.SHA256 {
							return errors.New("artifact checksum differs from recorded version")
						}
					}
				}
			}
		}

		if appID == "" {
			return nil
		}
		p, err := s.LoadPolicy(appID)
		if err != nil {
			return err
		}
		if p.Mode == "enforce" {
			return errors.New("artifact security metadata is missing; rebuild before deployment")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if appID != "" && r.AppID != appID {
		return errors.New("artifact security metadata belongs to a different application")
	}
	return Verify(ctx, s, r, path)
}

func VerifyApplicationPath(ctx context.Context, s *Store, path, appID, appName string) error {
	if !identityPattern.MatchString(appName) {
		return errors.New("invalid application name")
	}
	raw, err := readBounded(filepath.Join(s.DataDir, "apps", appName, "versions.json"), MaxReportBytes)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var versions struct {
		Versions []struct {
			Version         int     `json:"version"`
			SHA256          string  `json:"sha256"`
			Security        *Report `json:"security"`
			MatrixArtifacts []struct {
				Binary   string  `json:"binary"`
				SHA256   string  `json:"sha256"`
				Security *Report `json:"security"`
			} `json:"matrix_artifacts"`
		} `json:"versions"`
	}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &versions); err != nil {
			return err
		}
	}
	sidecar, sideErr := LoadSidecar(path)
	if sideErr != nil && !os.IsNotExist(sideErr) {
		return sideErr
	}
	if sidecar != nil && (sidecar.AppName != appName || (appID != "" && sidecar.AppID != appID)) {
		return errors.New("artifact security metadata belongs to a different application")
	}
	for _, v := range versions.Versions {
		for _, artifact := range v.MatrixArtifacts {
			if artifact.Binary == "" || filepath.Clean(artifact.Binary) != filepath.Clean(path) {
				continue
			}
			if artifact.SHA256 != "" {
				hash, _, err := SHA256File(path)
				if err != nil || hash != artifact.SHA256 {
					return errors.New("matrix artifact differs from recorded version digest")
				}
			}
			if artifact.Security != nil {
				copy := *artifact.Security
				if copy.AppName != appName || (appID != "" && copy.AppID != appID) {
					return errors.New("matrix artifact security metadata belongs to a different application")
				}
				return Verify(ctx, s, &copy, path)
			}
		}
		if sidecar != nil && v.Version == sidecar.Version {
			hash, _, err := SHA256File(path)
			if err != nil {
				return err
			}
			if v.SHA256 != "" && hash != v.SHA256 {
				return errors.New("artifact differs from recorded version digest")
			}
			if v.Security != nil {
				copy := *v.Security
				return Verify(ctx, s, &copy, path)
			}
		}
		if sidecar == nil && v.Security != nil {
			return errors.New("artifact security sidecar is missing; rebuild before launching")
		}
	}
	return VerifyPath(ctx, s, path, appID)
}
