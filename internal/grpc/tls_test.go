package grpc

// tls_test.go verifies the transport-security hardening of the backend
// channel:
//
//   - SPKI pinning: parse pins, match the serving certificate's public key,
//     reject mismatches.
//   - The insecure (plaintext) dev fallback can never be selected for the
//     production backend host, regardless of the configured mode.
//   - The production TLS config requires TLS >= 1.2.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"
)

// genTestCert produces a self-signed certificate and returns its DER encoding
// (the shape crypto/tls hands to VerifyPeerCertificate as rawCerts[0]).
func genTestCert(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "phelix-backend"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return der
}

func spkiPinOf(t *testing.T, der []byte) []byte {
	t.Helper()
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return sum[:]
}

func TestVerifySPKIPins(t *testing.T) {
	der := genTestCert(t)
	pin := spkiPinOf(t, der)

	if err := verifySPKIPins([][]byte{pin}, [][]byte{der}); err != nil {
		t.Fatalf("matching pin must verify, got: %v", err)
	}

	wrong := append([]byte(nil), pin...)
	wrong[0] ^= 0xFF
	if err := verifySPKIPins([][]byte{wrong}, [][]byte{der}); err == nil {
		t.Fatal("mismatched pin must fail verification")
	}

	// Multiple pins: any one match is enough (rotation overlap).
	if err := verifySPKIPins([][]byte{wrong, pin}, [][]byte{der}); err != nil {
		t.Fatalf("pin set containing the right pin must verify, got: %v", err)
	}

	// No certificate presented at all.
	if err := verifySPKIPins([][]byte{pin}, nil); err == nil {
		t.Fatal("empty certificate chain must fail pinning")
	}

	// Garbage certificate bytes must fail closed, not panic.
	if err := verifySPKIPins([][]byte{pin}, [][]byte{[]byte("not a certificate")}); err == nil {
		t.Fatal("unparseable certificate must fail pinning")
	}
}

func TestParseTLSPins(t *testing.T) {
	if got := parseTLSPins(""); got != nil {
		t.Fatalf("empty input must disable pinning, got %v", got)
	}
	if got := parseTLSPins("   "); got != nil {
		t.Fatalf("blank input must disable pinning, got %v", got)
	}

	valid := strings.Repeat("ab", 32) // 64 hex chars = 32 bytes
	got := parseTLSPins(valid)
	if len(got) != 1 {
		t.Fatalf("one valid pin expected, got %d", len(got))
	}

	// Comma-separated, colon-styled (openssl output), and mixed separators.
	colonStyled := strings.Repeat("cd:", 31) + "cd"
	got = parseTLSPins(valid + ", " + colonStyled + "\t" + valid)
	if len(got) != 3 {
		t.Fatalf("three valid pins expected, got %d", len(got))
	}

	// Invalid entries are skipped, valid ones kept.
	got = parseTLSPins("zzzz-not-hex, " + valid + ", tooshort")
	if len(got) != 1 {
		t.Fatalf("only the valid pin should survive, got %d", len(got))
	}

	// Only invalid input disables pinning entirely.
	if got := parseTLSPins("zzzz, 1234"); got != nil {
		t.Fatalf("all-invalid input must disable pinning, got %v", got)
	}
}

func TestAgentTLSConfig(t *testing.T) {
	t.Run("no pin configured", func(t *testing.T) {
		t.Setenv(tlsPinEnvVar, "")
		cfg := agentTLSConfig()
		if cfg.VerifyPeerCertificate != nil {
			t.Fatal("pinning must be off without the environment variable")
		}
		if cfg.MinVersion != tlsVersion12 {
			t.Fatalf("TLS 1.2 must be the minimum version, got %x", cfg.MinVersion)
		}
		if cfg.InsecureSkipVerify {
			t.Fatal("standard CA validation must never be skipped")
		}
	})

	t.Run("pin configured", func(t *testing.T) {
		pin := strings.Repeat("ab", 32)
		t.Setenv(tlsPinEnvVar, pin)
		cfg := agentTLSConfig()
		if cfg.VerifyPeerCertificate == nil {
			t.Fatal("pinning must be on when the environment variable is set")
		}
		// Wire the configured verifier end-to-end against a real certificate.
		der := genTestCert(t)
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse cert: %v", err)
		}
		sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
		t.Setenv(tlsPinEnvVar, hex.EncodeToString(sum[:]))
		cfg = agentTLSConfig()
		if err := cfg.VerifyPeerCertificate([][]byte{der}, nil); err != nil {
			t.Fatalf("configured pin must accept the matching certificate: %v", err)
		}
	})
}

// insecureTransport reports whether the credentials are the plaintext ones.
func insecureTransport(tc credentials.TransportCredentials) bool {
	return strings.Contains(fmt.Sprintf("%T", tc), "insecure")
}

func TestTransportCredentialsFor(t *testing.T) {
	cases := []struct {
		name      string
		mode      string
		target    string
		insecureW bool
	}{
		// Dev mode against a local backend keeps the plaintext fallback for
		// local development.
		{"dev + local backend", "dev", "phelix.local:50051", true},
		{"dev + loopback", "dev", "127.0.0.1:50051", true},
		// The production host is ALWAYS TLS, even if a build accidentally
		// ships dev mode.
		{"dev + production host", "dev", "phelix.anophel.com:443", false},
		{"dev + production host via scheme", "dev", "dns:///phelix.anophel.com:443", false},
		{"production mode", "production", "phelix.anophel.com:443", false},
		{"empty mode", "", "phelix.anophel.com:443", false},
		{"empty target", "production", "", false},
	}
	for _, tc := range cases {
		got := transportCredentialsFor(tc.mode, tc.target)
		if insecureTransport(got) != tc.insecureW {
			t.Errorf("%s: transportCredentialsFor(%q, %q) insecure=%v, want %v",
				tc.name, tc.mode, tc.target, insecureTransport(got), tc.insecureW)
		}
	}
}

func TestGrpcTargetHost(t *testing.T) {
	cases := map[string]string{
		"phelix.local:50051":            "phelix.local",
		"phelix.anophel.com:443":        "phelix.anophel.com",
		"phelix.anophel.com":            "phelix.anophel.com",
		"dns:///phelix.anophel.com:443": "phelix.anophel.com",
		"[::1]:50051":                   "::1",
		"PHelix.Anophel.COM:443":        "phelix.anophel.com",
	}
	for target, want := range cases {
		if got := grpcTargetHost(target); got != want {
			t.Errorf("grpcTargetHost(%q) = %q, want %q", target, got, want)
		}
	}
}

// tlsVersion12 avoids importing crypto/tls in this test file's main scope
// while keeping the constant readable in TestAgentTLSConfig.
const tlsVersion12 = 0x0303
