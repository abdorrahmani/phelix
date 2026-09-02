package builder

import (
	"bytes"
	"strings"
)

// maxToolOutput bounds the captured output kept on a ToolError. Compiler and
// tool output can be very large; the CLI error reporter only needs the most
// recent lines to classify known failures and show raw diagnostics.
const maxToolOutput = 8 * 1024

// ToolError reports a failed external build-tool invocation (go, cargo).
// It carries the tool identity and a bounded tail of the tool's captured
// output so the CLI error reporter can (a) classify known tool failures by
// evidence and (b) show raw diagnostics for unknown ones.
//
// ToolError is deliberately not a phelixerr.Error: builders wrap it with
// phelixerr.Wrapf, so the structured code stays on the outer error and the
// cause chain remains Error → ToolError → (typically) *exec.ExitError, keeping
// errors.Is / errors.As intact. The captured output is never part of any
// error message; it is rendered only after passing through phelixerr.Redact,
// and it must never be echoed unredacted into logs or other errors.
type ToolError struct {
	// Tool is the binary that failed, e.g. "go" or "cargo". It is a fixed
	// identity string set by the builder, never derived from tool output.
	Tool string
	// Output is the bounded tail of the tool's combined stdout+stderr.
	Output string
	// Err is the underlying failure, preserved so errors.Is/As still reach it.
	Err error
}

// Error returns the underlying failure message. The captured output is
// intentionally not part of the message.
func (e *ToolError) Error() string {
	if e == nil || e.Err == nil {
		return "build tool failed"
	}
	return e.Err.Error()
}

// Unwrap exposes the underlying failure (e.g. *exec.ExitError).
func (e *ToolError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// tailOutput keeps at most maxToolOutput bytes from the end of out, trimmed
// to start on a line boundary so the retained tail is a whole number of lines.
func tailOutput(out []byte) string {
	if len(out) > maxToolOutput {
		out = out[len(out)-maxToolOutput:]
		if i := bytes.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}
	}
	return strings.TrimLeft(string(out), "\n")
}
