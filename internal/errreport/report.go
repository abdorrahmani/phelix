// Package errreport enriches Phelix's structured errors with human-readable
// explanations, concrete fix suggestions, real commands, and documentation
// links for a small registry of known, recognizable failure modes.
//
// It is an additional presentation layer at the CLI render boundary — not a
// parallel error system. Classification, exit codes, redaction, and
// root-cause preservation remain the responsibility of internal/errors
// (phelixerr) and cmd.RenderError. Errors the registry does not recognize are
// rendered exactly as before, with their raw cause untouched.
//
// Detection is evidence-based: resolvers match on the existing structured
// codes (phelixerr.IsCode), typed error values (errors.As), tool identity,
// and — only where no typed signal exists — narrow, tested tokens in the
// wrap-chain text or captured tool output. Bare words like "error" or
// "failed" never trigger a classification.
//
// Security: report fields are rendered only after passing through
// phelixerr.Redact. Commands are static strings (at most parameterized by a
// validated integer, e.g. a port) — they are never generated from untrusted
// error text, and Phelix never executes them on the user's behalf.
package errreport

import (
	"errors"
	"strings"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Report is the enriched, user-facing description of a known error. It layers
// explanation and fix guidance on top of the existing structured error: the
// error's code, message, exit code, cause chain, and redaction guarantees are
// unchanged.
type Report struct {
	// Code echoes the structured code the error already carries. Resolvers
	// never remap it; the exit code continues to come from cmd.ExitCodeFor.
	Code phelixerr.Code
	// Title names the problem in a few words ("Go toolchain not found").
	Title string
	// Explanation says, in simple language, what went wrong and why.
	Explanation string
	// Suggestion describes the concrete fix.
	Suggestion string
	// Command is a real, safe, platform-appropriate command the user can run
	// themselves. Empty when no deterministic command exists for the problem —
	// a missing command must never be faked with a placeholder.
	Command string
	// DocsURL points at existing Phelix documentation when the problem is
	// Phelix-specific, otherwise at official upstream documentation.
	DocsURL string
}

// Resolver reports whether it recognizes err and, if so, returns the Report.
// Resolvers must be cheap (called on every rendered error) and must never
// mutate the error.
type Resolver interface {
	Resolve(err error) (Report, bool)
}

// resolvers is the ordered known-error registry: the first match wins, so
// more specific resolvers come first. Register prepends to keep newly added
// resolvers overridable in the same spirit.
var resolvers = []Resolver{
	toolchainResolver{},
	portResolver{},
	goModuleResolver{},
}

// Register adds r to the front of the registry. It exists so new known errors
// can be added without editing the core list (and for tests); production
// code rarely needs it.
func Register(r Resolver) {
	resolvers = append([]Resolver{r}, resolvers...)
}

// For walks the registry and returns the Report for the first resolver that
// recognizes err. ok is false for unknown errors, which must be rendered the
// usual way (raw message, generic hint, --debug cause).
func For(err error) (Report, bool) {
	if err == nil {
		return Report{}, false
	}
	for _, r := range resolvers {
		if rep, ok := r.Resolve(err); ok {
			if rep.Code == "" {
				rep.Code = phelixerr.CodeOf(err)
			}
			return rep, true
		}
	}
	return Report{}, false
}

// ToolOutput returns the bounded, captured output of the failed external
// build tool carried by err, if any. It backs the raw-diagnostics section of
// the CLI renderer for unknown build failures.
func ToolOutput(err error) (string, bool) {
	var te *builder.ToolError
	if errors.As(err, &te) && te.Output != "" {
		return te.Output, true
	}
	return "", false
}

// chainText concatenates the messages of every error in the wrap chain.
// phelixerr.Error.Error() returns only its own message, so matching on
// err.Error() alone would miss the context carried by inner errors.
func chainText(err error) string {
	var b strings.Builder
	for err != nil {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(err.Error())
		err = errors.Unwrap(err)
	}
	return b.String()
}

// containsAny reports whether s contains any of the tokens, case-insensitively.
func containsAny(s string, tokens ...string) bool {
	low := strings.ToLower(s)
	for _, t := range tokens {
		if strings.Contains(low, strings.ToLower(t)) {
			return true
		}
	}
	return false
}
