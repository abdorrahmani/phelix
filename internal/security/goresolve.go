package security

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// GoResolveTimeout bounds one module-resolution subprocess. A cold module
// cache with GOPROXY=off fails fast; a warm cache answers quickly.
const GoResolveTimeout = 60 * time.Second

// ResolvedModule is one module of the Go build list as the toolchain
// selects it: the authoritative answer to "which version does this
// application compile with".
type ResolvedModule struct {
	// Path is the effective module identity: the replacement target when
	// the module is replaced, the module path otherwise.
	Path string
	// Version is the selected version, empty for local replacements.
	Version string
	// Indirect marks modules no scanned manifest requires directly.
	Indirect bool
	// ReplacedFrom is the original module path for replaced modules.
	ReplacedFrom string
	// LocalReplace marks a replacement without a pinned version.
	LocalReplace bool
}

// GoResolver resolves the selected build list of a Go project. The
// production implementation shells out to the Go toolchain with the proxy
// disabled so resolution never touches the network; tests substitute
// fakes.
type GoResolver interface {
	ResolveModules(ctx context.Context, dir string) ([]ResolvedModule, error)
	// ModuleGraph returns the requirement edges of the module graph as
	// (parent, child) name@version pairs, with requirement versions that
	// callers map onto selected versions.
	ModuleGraph(ctx context.Context, dir string) ([]ModuleGraphEdge, error)
}

// ModuleGraphEdge is one raw requirement edge from `go mod graph`:
// Parent@ParentVersion requires Child@ChildVersion. Versions are the
// requirement versions, not necessarily the selected ones.
type ModuleGraphEdge struct {
	ParentPath    string
	ParentVersion string
	ChildPath     string
	ChildVersion  string
	ParentIsMain  bool
}

// ToolchainGoResolver resolves modules with `go list -m -json all`. The
// subprocess runs with GOPROXY=off: resolution uses only the local module
// cache and fails explicitly when modules are missing, never downloading.
// A failed resolution is not a scan failure; callers degrade to manifest
// evidence and say so.
type ToolchainGoResolver struct {
	// LookPath resolves executables. Overridable for tests.
	LookPath func(string) (string, error)
	// CommandContext creates executed commands. Overridable for tests.
	CommandContext func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// ResolveModules returns the selected build list for dir.
func (r *ToolchainGoResolver) ResolveModules(ctx context.Context, dir string) ([]ResolvedModule, error) {
	if _, err := r.lookPath("go"); err != nil {
		return nil, phelixerr.New(phelixerr.CodeToolchainNotFound,
			"the Go toolchain is required to resolve selected module versions; install Go or review the manifest-only inventory")
	}
	ctx, cancel := context.WithTimeout(ctx, GoResolveTimeout)
	defer cancel()

	cmd := r.command(ctx, "go", "list", "-m", "-json", "all")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		const maxDetail = 400
		if len(detail) > maxDetail {
			detail = detail[:maxDetail]
		}
		return nil, phelixerr.Newf(phelixerr.CodeToolchainNotFound,
			"go list -m could not resolve the build list (%s)", detail)
	}

	var modules []ResolvedModule
	decoder := json.NewDecoder(&stdout)
	for {
		var entry goListModule
		if err := decoder.Decode(&entry); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, phelixerr.Wrap(phelixerr.CodeToolchainNotFound, "go list -m produced unreadable output", err)
		}
		if entry.Main {
			continue
		}
		mod := ResolvedModule{Path: entry.Path, Version: entry.Version, Indirect: entry.Indirect}
		if entry.Replace != nil {
			mod.ReplacedFrom = entry.Path
			mod.Path = entry.Replace.Path
			mod.Version = entry.Replace.Version
			mod.LocalReplace = entry.Replace.Version == ""
		}
		modules = append(modules, mod)
	}
	return modules, nil
}

func (r *ToolchainGoResolver) lookPath(name string) (string, error) {
	if r.LookPath != nil {
		return r.LookPath(name)
	}
	return exec.LookPath(name)
}

func (r *ToolchainGoResolver) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if r.CommandContext != nil {
		return r.CommandContext(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// ModuleGraph returns the raw requirement edges of the module graph.
func (r *ToolchainGoResolver) ModuleGraph(ctx context.Context, dir string) ([]ModuleGraphEdge, error) {
	if _, err := r.lookPath("go"); err != nil {
		return nil, phelixerr.New(phelixerr.CodeToolchainNotFound,
			"the Go toolchain is required to resolve the module graph")
	}
	ctx, cancel := context.WithTimeout(ctx, GoResolveTimeout)
	defer cancel()

	cmd := r.command(ctx, "go", "mod", "graph")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		const maxDetail = 400
		if len(detail) > maxDetail {
			detail = detail[:maxDetail]
		}
		return nil, phelixerr.Newf(phelixerr.CodeToolchainNotFound,
			"go mod graph could not resolve the module graph (%s)", detail)
	}

	var edges []ModuleGraphEdge
	for _, line := range strings.Split(stdout.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		parentPath, parentVersion := splitModuleVersion(fields[0])
		childPath, childVersion := splitModuleVersion(fields[1])
		edges = append(edges, ModuleGraphEdge{
			ParentPath:    parentPath,
			ParentVersion: parentVersion,
			ChildPath:     childPath,
			ChildVersion:  childVersion,
		})
	}
	return edges, nil
}

// splitModuleVersion splits a "path@version" graph node into its parts.
func splitModuleVersion(node string) (string, string) {
	if i := strings.IndexByte(node, '@'); i >= 0 {
		return node[:i], node[i+1:]
	}
	return node, ""
}

// goListModule mirrors one JSON object of `go list -m -json all`.
type goListModule struct {
	Path     string           `json:"Path"`
	Version  string           `json:"Version"`
	Indirect bool             `json:"Indirect"`
	Main     bool             `json:"Main"`
	Replace  *goListModuleRef `json:"Replace"`
}

type goListModuleRef struct {
	Path    string `json:"Path"`
	Version string `json:"Version"`
}

// resolvedModuleName is the inventory key for one resolved module.
func resolvedModuleName(mod ResolvedModule) string {
	return mod.Path
}
