package phelixerr

import (
	"regexp"
	"strings"
)

// credentialPatterns are conservative, well-known shapes of credentials and
// tokens that must never appear in rendered error or log output. Redaction is
// applied by the CLI renderer (and is available to logging helpers) so that
// even in --debug mode, where root causes are shown, secrets stay hidden.
//
// This is intentionally a bounded, deterministic list — it is not a general
// entropy detector. The first line of defence is that Phelix-authored error
// messages never embed secrets at all; redaction is a second, coarse net.
// The scanner keys ("token", "key", "secret", "password", "authorization")
// are case-insensitive.
var credentialPatterns = []string{
	"sk-",          // OpenAI-style API keys
	"pk-",          // publishable/stripe-style keys
	"ghp_", "gho_", // GitHub personal access tokens
	"ghu_", "ghs_", // GitHub user/server tokens
	"xoxb-", "xoxp-", // Slack tokens
	"AKIA",      // AWS access key id prefix
	"ASIA",      // AWS temporary access key prefix
	"eyJ",       // JWT header base64 (long strings later truncated)
	"Bearer ",   // Authorization: Bearer <token>
	"X-API-Key", // header name (value trimming handled below)
	"apiKey=",
	"apikey=",
	"password=",
	"token=",
	"secret=",
}

// urlCredentialPattern matches credentials embedded in connection-string
// URLs — the shape application logs most commonly leak (DATABASE_URL and
// friends). The userinfo password is masked; scheme, user and host stay
// readable so the log line remains useful for debugging. The username may be
// empty (redis://:password@host).
var urlCredentialPattern = regexp.MustCompile(
	`(?i)\b((?:postgres(?:ql)?|mysql|mariadb|mongodb(?:\+srv)?|redis|rediss|amqps?|ftp|ftps|sftp|smtps?|imaps?|pop3s?)://)([^/@\s:]*):([^/@\s]+)@`)

// pemPrivateKeyPattern matches a full PEM private-key block, including
// multi-line ones. The key body is replaced with a marker; the block type
// header is kept so the log line still says what was leaked.
var pemPrivateKeyPattern = regexp.MustCompile(
	`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?s:.)*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

// Redact scrubs obvious credential material from a string so it is safe to
// render. It masks the matched region (and anything that looks like the value
// that followed a key= marker) with "***". Deterministic and lossy-on-purpose:
// calling Redact on already-safe text returns it unchanged.
func Redact(s string) string {
	if s == "" {
		return s
	}
	out := s
	for _, pat := range credentialPatterns {
		idx := 0
		lowPat := strings.ToLower(pat)
		lowOut := strings.ToLower(out)
		for {
			pos := strings.Index(lowOut[idx:], lowPat)
			if pos < 0 {
				break
			}
			pos += idx
			end := pos + len(pat)
			// If this is a key= style marker, mask the rest of the token too.
			if pat[len(pat)-1] == '=' || strings.EqualFold(pat, "Bearer ") {
				j := end
				for j < len(out) && (isTokenChar(out[j]) || out[j] == '.' || out[j] == '-' || out[j] == '_') {
					j++
				}
				end = j
			}
			out = out[:pos] + "***" + out[end:]
			lowOut = strings.ToLower(out)
			idx = pos + 3
			if idx >= len(out) {
				break
			}
		}
	}
	// Connection-string URLs: keep scheme://user: but mask the password.
	out = urlCredentialPattern.ReplaceAllString(out, "$1$2:***@")
	// PEM private-key blocks: drop the entire key body.
	out = pemPrivateKeyPattern.ReplaceAllString(out, "-----BEGIN PRIVATE KEY-----[REDACTED]")
	return out
}

// RedactCause returns a redacted rendering of an error chain for debug output.
// It prints the structured message then, when present, the deepest cause —
// both run through Redact. Messages authored by Phelix are already safe; this
// defends against a lower layer embedding a raw credential in its error text.
func RedactCause(err error) string {
	if err == nil {
		return ""
	}
	msg := Redact(err.Error())
	lines := []string{"  " + msg}
	inner := Cause(err)
	if inner != nil && inner != err {
		lines = append(lines, "  root cause: "+Redact(inner.Error()))
	}
	return strings.Join(lines, "\n")
}

func isTokenChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
