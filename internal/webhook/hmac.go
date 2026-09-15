package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// SignatureHeader carries the GitHub-style HMAC-SHA256 signature of the raw
// request body: "sha256=<hex>".
const SignatureHeader = "X-Hub-Signature-256"

// DeliveryHeader carries the provider's unique delivery identifier, used for
// replay protection (a redelivered request must not rebuild twice).
const DeliveryHeader = "X-GitHub-Delivery"

// EventTypeHeader carries the provider's event type ("push", "ping", ...).
const EventTypeHeader = "X-GitHub-Event"

// ProviderGitHub is the only payload provider supported by this
// implementation; recorded on every job as metadata.
const ProviderGitHub = "github"

const signaturePrefix = "sha256="

// VerifySignature reports whether sigHeader authenticates the exact raw body
// under secret. It is the single authentication gate for webhook requests:
//
//   - the HMAC is computed over the raw bytes exactly as received (callers
//     must not re-encode or trim the body before verifying);
//   - the digest is compared with hmac.Equal (constant time) — never with ==;
//   - a missing, malformed, wrongly-prefixed or wrongly-sized signature is
//     rejected just like an incorrect one.
func VerifySignature(secret, body []byte, sigHeader string) bool {
	if len(secret) == 0 {
		return false
	}
	if !strings.HasPrefix(sigHeader, signaturePrefix) {
		return false
	}
	hexDigest := sigHeader[len(signaturePrefix):]
	if len(hexDigest) != 2*sha256.Size {
		return false
	}
	given, err := hex.DecodeString(hexDigest)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), given)
}
