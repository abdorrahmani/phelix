package matrix

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Artifact checksums: every successful matrix artifact carries a SHA-256
// computed from its final bytes, so a release set can be verified after the
// fact (did the artifact change since it was built?) and so the release
// manifest can reference artifacts unambiguously.
//
// This is an integrity mechanism, not an authenticity one: the checksum
// answers "are these the bytes that were built?", never "who built them?".

// SHA256HexLen is the length of a hexadecimal SHA-256 digest.
const SHA256HexLen = sha256.Size * 2

// shortSHALen is how many leading characters terminal output shows; the full
// digest is always available in JSON output and `matrix show`.
const shortSHALen = 12

// ShortSHA256 renders a checksum for compact terminal output. Values shorter
// than the short form (malformed or absent) are returned unchanged.
func ShortSHA256(sum string) string {
	if len(sum) <= shortSHALen {
		return sum
	}
	return sum[:shortSHALen] + "…"
}

// SHA256File computes the SHA-256 checksum of a file by streaming its bytes —
// artifacts are never loaded into memory wholesale, so multi-hundred-megabyte
// binaries cost no more than a small read buffer. The digest is returned as
// lowercase hex.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "open artifact %s", path)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "read artifact %s", path)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// finalizeArtifactChecksum attaches the SHA-256 of the final artifact to a
// successful result. It must be called only after the artifact has been fully
// produced and finalized (the builders do this immediately before returning
// success).
//
// A checksum that cannot be computed is an artifact-integrity failure: the
// result is converted to failed — an artifact whose bytes cannot be verified
// must never be recorded as a valid release artifact. The error carries a
// non-transient code (CodeBuildFailed) so the automatic-retry classifier does
// not re-run a deterministic checksum failure.
func finalizeArtifactChecksum(res *Result) {
	if res == nil || res.Status != "success" || res.Artifact == "" {
		return
	}
	sum, err := SHA256File(res.Artifact)
	if err != nil {
		res.Status = "failed"
		res.SHA256 = ""
		res.Error = phelixerr.Newf(phelixerr.CodeBuildFailed,
			"artifact integrity check failed for %s: %v — the built artifact could not be checksummed and is not recorded as a valid release artifact",
			res.Combination.ID(), err)
		return
	}
	res.SHA256 = sum
}
