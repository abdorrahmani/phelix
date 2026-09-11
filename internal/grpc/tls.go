package grpc

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"os"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
)

// tlsPinEnvVar opts into TLS certificate pinning for the backend connection.
// When set to a comma- or space-separated list of hex-encoded SHA-256 hashes
// of the backend certificate's Subject Public Key Info (SPKI), the client
// additionally requires the server's leaf SPKI to match one of the pins, on
// top of standard CA validation. Pinning the SPKI (rather than the full
// certificate) survives backend certificate renewals as long as the key pair
// is reused.
//
// Generate a pin from the serving certificate with:
//
//	openssl x509 -in backend.pem -pubkey -noout \
//	  | openssl pkey -pubin -outform DER \
//	  | sha256sum
const tlsPinEnvVar = "PHELIX_TLS_PIN_SHA256"

// productionBackendHosts lists backend hosts that must never be contacted
// over an insecure (plaintext) transport, regardless of the configured mode.
// This is the fail-safe against a production release accidentally shipping
// the dev-mode embedded config: even with mode "dev", these hosts force TLS.
var productionBackendHosts = map[string]struct{}{
	"phelix.anophel.com": {},
}

// agentTLSConfig returns the TLS configuration for the backend connection.
// Standard WebPKI validation against the system trust store is always on
// (there is no InsecureSkipVerify anywhere in this codebase); when the
// pinning environment variable is set, SPKI pinning is layered on top so a
// MITM with a publicly-trusted certificate is also rejected.
func agentTLSConfig() *tls.Config {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	pins := parseTLSPins(os.Getenv(tlsPinEnvVar))
	if len(pins) > 0 {
		logs.InfoFile("grpc", "[gRPC] TLS certificate pinning enabled (%d SPKI pin(s))", len(pins))
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifySPKIPins(pins, rawCerts)
		}
	}
	return cfg
}

// parseTLSPins parses a comma- or space-separated list of hex-encoded SHA-256
// SPKI pins. Colons inside the hex are tolerated (openssl-style output).
// Invalid entries are skipped with a warning rather than failing the whole
// set — but an entirely empty result means pinning stays off.
func parseTLSPins(raw string) [][]byte {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var pins [][]byte
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		hexStr := strings.ReplaceAll(field, ":", "")
		b, err := hex.DecodeString(hexStr)
		if err != nil || len(b) != sha256.Size {
			logs.WarningFile("grpc", "[gRPC] ignoring malformed TLS pin (expected %d hex-encoded bytes): %q", sha256.Size, field)
			continue
		}
		pins = append(pins, b)
	}
	return pins
}

// verifySPKIPins checks that the presented leaf certificate's SPKI SHA-256
// matches at least one pin. It only runs after crypto/tls has already
// validated the certificate chain — it tightens validation, never replaces
// it.
func verifySPKIPins(pins [][]byte, rawCerts [][]byte) error {
	if len(rawCerts) == 0 {
		return phelixerr.New(phelixerr.CodeConnection, "TLS pinning: server presented no certificate")
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConnection, "TLS pinning: cannot parse server certificate", err)
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	for _, pin := range pins {
		if bytes.Equal(sum[:], pin) {
			return nil
		}
	}
	return phelixerr.New(
		phelixerr.CodeConnection,
		"TLS pinning: server certificate does not match any pinned SPKI hash (possible MITM or backend key rotation)",
	)
}

// grpcTargetHost extracts the lowercase host portion of a gRPC target such
// as "phelix.anophel.com:443" or "dns:///phelix.local:50051". IPv6 targets
// in bracket form ("[::1]:50051") return the bracketed host without the
// port.
func grpcTargetHost(target string) string {
	if i := strings.Index(target, "://"); i >= 0 {
		target = target[i+3:]
	}
	// "dns:///host:port" carries an empty authority, so the endpoint sits
	// behind extra slashes after the scheme is stripped.
	target = strings.TrimLeft(target, "/")
	if i := strings.Index(target, "/"); i >= 0 {
		target = target[:i]
	}
	if strings.HasPrefix(target, "[") {
		if i := strings.Index(target, "]"); i >= 0 {
			return strings.ToLower(target[1:i])
		}
	}
	if i := strings.LastIndex(target, ":"); i >= 0 {
		target = target[:i]
	}
	return strings.ToLower(target)
}

// isProductionBackend reports whether the target points at a host that must
// never be reached over plaintext.
func isProductionBackend(target string) bool {
	_, ok := productionBackendHosts[grpcTargetHost(target)]
	return ok
}
