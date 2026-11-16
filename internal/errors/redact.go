package phelixerr

import (
	"regexp"
	"strings"
)

var credentialPattern = regexp.MustCompile(
	`(?i:sk-|pk-|gh[pous]_|xox[bp]-|AKIA|ASIA|eyJ|Bearer |X-API-Key|apikey=|password=|token=|secret=)[a-zA-Z0-9._-]*`)

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
	out := applyRegisteredValues(s)
	out = credentialPattern.ReplaceAllString(out, "***")
	if strings.Contains(out, "://") {
		out = urlCredentialPattern.ReplaceAllString(out, "$1$2:***@")
	}
	if strings.Contains(out, "PRIVATE KEY-----") {
		out = pemPrivateKeyPattern.ReplaceAllString(out, "-----BEGIN PRIVATE KEY-----[REDACTED]")
	}
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
