package phelixerr

import (
	"errors"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := []struct {
		in       string
		mustNot  []string
		mustHave []string
	}{
		{
			in:      "sk-abcdef1234567890",
			mustNot: []string{"sk-abcdef1234567890"},
		},
		{
			in:      "ghp_abcdef1234567890",
			mustNot: []string{"ghp_abcdef1234567890"},
		},
		{
			in:       "error: password=hunter2 refused",
			mustNot:  []string{"hunter2", "password=hunter2"},
			mustHave: []string{"refused"}, // surrounding context preserved
		},
		{
			in:       "Authorization: Bearer " + jwtBase64("HS256") + ".rest",
			mustNot:  []string{jwtBase64("HS256") + ".rest"},
			mustHave: []string{"Authorization:", "***"},
		},
		{
			in:       "plain filesystem error: /tmp/x: no such file",
			mustHave: []string{"plain filesystem error: /tmp/x: no such file"},
		},
	}
	for _, tc := range cases {
		got := Redact(tc.in)
		for _, s := range tc.mustNot {
			if strings.Contains(got, s) {
				t.Fatalf("Redact(%q) = %q, must NOT contain %q", tc.in, got, s)
			}
		}
		for _, s := range tc.mustHave {
			if !strings.Contains(got, s) {
				t.Fatalf("Redact(%q) = %q, must contain %q", tc.in, got, s)
			}
		}
	}
}

func TestRedactIsDeterministicAndStable(t *testing.T) {
	in := "failed connecting with token=abc123xyz"
	if Redact(in) != Redact(in) {
		t.Fatal("redaction must be deterministic")
	}
	// Redacting already-redacted text must not change it further.
	once := Redact(in)
	if Redact(once) != once {
		t.Fatal("redaction must be idempotent")
	}
}

func TestRedactCause(t *testing.T) {
	root := errors.New("connect: password=secretpw timeout")
	err := Wrap(CodeConnection, "failed to connect", root)
	got := RedactCause(err)
	if strings.Contains(got, "secretpw") {
		t.Fatalf("cause redaction leaked value: %q", got)
	}
	if !strings.Contains(strings.ToLower(got), "failed to connect") {
		t.Fatalf("cause redaction dropped message: %q", got)
	}
}

func TestRedact_ConnectionStringURLs(t *testing.T) {
	cases := []struct {
		in       string
		mustNot  []string
		mustHave []string
	}{
		{
			// The canonical DATABASE_URL leak in an application log.
			in:       "db connect failed: DATABASE_URL=postgres://admin:hunter2@db.internal:5432/prod",
			mustNot:  []string{"hunter2"},
			mustHave: []string{"postgres://admin:***@db.internal:5432/prod"},
		},
		{
			in:       "mongodb+srv://svc:pa55word@cluster0.abcd.mongodb.net/app?retryWrites=true",
			mustNot:  []string{"pa55word"},
			mustHave: []string{"mongodb+srv://svc:***@"},
		},
		{
			// redis URLs commonly carry an empty username.
			in:      "cache dial: redis://:redispass@cache.internal:6379/0",
			mustNot: []string{"redispass"},
		},
		{
			in:      "amqp://guest:rabbitpw@mq.internal:5672/vhost",
			mustNot: []string{"rabbitpw"},
		},
		{
			in:      "mysql://root:sqlpass@127.0.0.1:3306/shop",
			mustNot: []string{"sqlpass"},
		},
		{
			// A URL without a password must pass through untouched.
			in:       "postgres://admin@db.internal:5432/prod connected",
			mustHave: []string{"postgres://admin@db.internal:5432/prod"},
		},
		{
			// A word ending in a scheme name is not a URL.
			in:       "checked xpostgres://not:a:match okay",
			mustHave: []string{"xpostgres://not:a:match"},
		},
	}
	for _, tc := range cases {
		got := Redact(tc.in)
		for _, s := range tc.mustNot {
			if strings.Contains(got, s) {
				t.Fatalf("Redact(%q) = %q, must NOT contain %q", tc.in, got, s)
			}
		}
		for _, s := range tc.mustHave {
			if !strings.Contains(got, s) {
				t.Fatalf("Redact(%q) = %q, must contain %q", tc.in, got, s)
			}
		}
		// The new patterns must keep redaction idempotent.
		if again := Redact(got); again != got {
			t.Fatalf("Redact not idempotent for %q: %q -> %q", tc.in, got, again)
		}
	}
}

func TestRedact_PEMPrivateKey(t *testing.T) {
	in := "loaded key:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA0h3secretkeybody\nline2morebody\n-----END RSA PRIVATE KEY-----\nready"
	got := Redact(in)
	if strings.Contains(got, "MIIEpAIBAAKCAQEA0h3secretkeybody") {
		t.Fatalf("PEM private key body leaked: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("PEM block must be replaced with a redaction marker: %q", got)
	}
	if !strings.Contains(got, "loaded key:") || !strings.Contains(got, "ready") {
		t.Fatalf("surrounding log context must be preserved: %q", got)
	}
	if again := Redact(got); again != got {
		t.Fatalf("PEM redaction not idempotent: %q -> %q", got, again)
	}
}

// jwtBase64 is the deterministic base64url header "eyJ…" that opens a JWT, so
// the redactor's JWT pattern is exercised with a real header shape.
func jwtBase64(alg string) string {
	// base64url(`{"alg":"` + alg + `"}`), no padding.
	const b64url = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	header := []byte(`{"alg":"` + alg + `"}`)
	var out []byte
	for i := 0; i < len(header); i += 3 {
		var n uint32 = uint32(header[i]) << 16
		if i+1 < len(header) {
			n |= uint32(header[i+1]) << 8
		}
		if i+2 < len(header) {
			n |= uint32(header[i+2])
		}
		out = append(out, b64url[(n>>18)&63], b64url[(n>>12)&63])
		if i+1 < len(header) {
			out = append(out, b64url[(n>>6)&63])
		}
		if i+2 < len(header) {
			out = append(out, b64url[n&63])
		}
	}
	return string(out)
}
