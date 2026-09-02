package errreport

import (
	"errors"
	"strings"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// goModuleResolver classifies known Go module / go.mod failures from the
// captured output of a failed `go build`. Detection requires, in order:
//
//  1. the structured BUILD_FAILED code,
//  2. a builder.ToolError in the chain (typed evidence, via errors.As) whose
//     Tool is exactly "go" — a fixed identity set by the builder, and
//  3. the captured go tool output to match one of the narrow, documented
//     conditions below.
//
// Output that matches none of them stays unknown: a compiler error such as
// "undefined: foo" must not be mis-explained or handed a generic fix. Only
// the go tool's own output is inspected; bare words like "error" or "failed"
// never trigger a classification.
type goModuleResolver struct{}

func (goModuleResolver) Resolve(err error) (Report, bool) {
	if !phelixerr.IsCode(err, phelixerr.CodeBuildFailed) {
		return Report{}, false
	}
	var te *builder.ToolError
	if !errors.As(err, &te) || te.Tool != "go" {
		return Report{}, false
	}
	return classifyGoModuleOutput(te.Output)
}

// classifyGoModuleOutput maps a known go-module failure to its report. The
// cases and their fixes are the documented remedies for each specific
// condition — go mod tidy is suggested only for the conditions it actually
// fixes (go.sum drift, undeclared dependencies), never as a catch-all.
func classifyGoModuleOutput(output string) (Report, bool) {
	out := strings.ToLower(output)
	switch {
	case containsAny(out, "go.mod file not found"):
		return Report{
			Code:        phelixerr.CodeBuildFailed,
			Title:       "go.mod file not found",
			Explanation: "The project has Go source files but no go.mod, so the go tool does not treat the directory as a module and cannot resolve its dependencies.",
			Suggestion:  "Initialize a Go module in the project root. If go mod init cannot infer a module path, pass one explicitly, e.g. go mod init github.com/you/myapp.",
			Command:     "go mod init",
			DocsURL:     "https://go.dev/ref/mod",
		}, true

	case containsAny(out, "malformed go.mod", "unknown directive", "parsing go.mod", "malformed module path"):
		return Report{
			Code:        phelixerr.CodeBuildFailed,
			Title:       "Invalid go.mod",
			Explanation: "go.mod could not be parsed — it likely contains a syntax error or an unsupported directive at the line named in the go tool output below.",
			Suggestion:  "Fix the reported line in go.mod, then build again. Once the file parses, go mod edit -fmt reformats it.",
			DocsURL:     "https://go.dev/ref/mod",
		}, true

	case containsAny(out, "missing go.sum entry"):
		return Report{
			Code:        phelixerr.CodeBuildFailed,
			Title:       "Missing go.sum entry",
			Explanation: "go.mod and go.sum are out of sync: the build uses a dependency whose checksum is not recorded in go.sum.",
			Suggestion:  "Reconcile the module set with the code's actual imports; this updates go.mod and go.sum.",
			Command:     "go mod tidy",
			DocsURL:     "https://go.dev/ref/mod",
		}, true

	case containsAny(out, "no required module provides package", "cannot find module providing package"):
		return Report{
			Code:        phelixerr.CodeBuildFailed,
			Title:       "Missing Go dependency",
			Explanation: "The code imports a package that is not provided by any module required in go.mod.",
			Suggestion:  "Record the missing dependency in go.mod; go mod tidy adds the requirements the code actually imports.",
			Command:     "go mod tidy",
			DocsURL:     "https://go.dev/ref/mod",
		}, true

	case containsAny(out, "checksum mismatch", "hash mismatch", "security error"):
		return Report{
			Code:        phelixerr.CodeBuildFailed,
			Title:       "Module checksum mismatch",
			Explanation: "A downloaded module does not match its expected checksum — most often a corrupt module-cache entry, occasionally a changed upstream release.",
			Suggestion:  "Clear the local module download cache so Go re-downloads verified copies. This only deletes cached module downloads, not your project.",
			Command:     "go clean -modcache && go mod download",
			DocsURL:     "https://go.dev/ref/mod",
		}, true

	case containsAny(out, "unknown revision", "invalid module version", "invalid version"):
		return Report{
			Code:        phelixerr.CodeBuildFailed,
			Title:       "Invalid module version",
			Explanation: "A version referenced in go.mod does not exist for its module — it may have been deleted upstream or mistyped.",
			Suggestion:  "Update the requirement for the module named in the go tool output below to a version that exists, e.g. go get <module>@latest.",
			DocsURL:     "https://go.dev/ref/mod",
		}, true

	case containsAny(out, "module lookup disabled"):
		return Report{
			Code:        phelixerr.CodeBuildFailed,
			Title:       "Module lookup disabled",
			Explanation: "Go is configured not to download modules (for example GOPROXY=off), so a dependency the build needs cannot be fetched.",
			Suggestion:  "Review the GOPROXY / GOFLAGS environment settings; allow downloads via a module proxy or GOPROXY=direct for the affected module.",
			DocsURL:     "https://go.dev/ref/mod",
		}, true
	}
	return Report{}, false
}
