package supplychain

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/abdorrahmani/phelix/internal/cargo"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/github/go-spdx/v2/spdxexp"
	"github.com/google/licensecheck"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func BuildInventory(ctx context.Context, dir, language string, args []string) (*security.Inventory, []LicenseFinding, map[string]string, error) {
	if language == "rust" {
		inv, licenses, err := rustInventory(ctx, dir, args)
		return inv, licenses, rustPackageHashes(dir), err
	}
	if language != "go" {
		return nil, nil, nil, errors.New("supply chain inventory supports Go and Rust native builds")
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); os.IsNotExist(err) {
		if _, workErr := os.Stat(filepath.Join(dir, "go.work")); os.IsNotExist(workErr) {
			return &security.Inventory{Ecosystem: security.EcosystemGo, Level: security.InventoryResolved, Modules: []security.Module{}, Roots: []string{"application"}}, []LicenseFinding{}, nil, nil
		}
	}
	resolver := &capturedGoResolver{}
	inv, err := security.InventoryGoWithResolution(ctx, dir, resolver)
	if err != nil {
		return nil, nil, nil, err
	}
	licenses, hashes := goLicenses(inv, resolver.Modules)
	return inv, licenses, hashes, nil
}

type capturedGoResolver struct{ Modules []security.ResolvedModule }

func (r *capturedGoResolver) ResolveModules(ctx context.Context, dir string) ([]security.ResolvedModule, error) {
	mods, err := (&security.ToolchainGoResolver{}).ResolveModules(ctx, dir)
	r.Modules = mods
	return mods, err
}
func (r *capturedGoResolver) ModuleGraph(ctx context.Context, dir string) ([]security.ModuleGraphEdge, error) {
	return (&security.ToolchainGoResolver{}).ModuleGraph(ctx, dir)
}

func goLicenses(inv *security.Inventory, modules []security.ResolvedModule) ([]LicenseFinding, map[string]string) {
	dirs := map[string]string{}
	hashes := map[string]string{}
	for _, m := range modules {
		key := security.ModuleKey(m.Path, m.Version)
		dirs[key] = m.Dir
		if strings.HasPrefix(m.Sum, "h1:") {
			digest, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(m.Sum, "h1:"))
			if err == nil && len(digest) == 32 {
				hashes[key] = hex.EncodeToString(digest)
			}
		}
	}
	out := make([]LicenseFinding, 0, len(inv.Modules))
	for _, m := range inv.Modules {
		l := LicenseFinding{Package: m.Name, Version: m.Version, Status: "unknown", Reason: "dependency license metadata is unavailable; manual review is required"}
		if root := dirs[m.Key]; root != "" {
			for _, name := range []string{"LICENSE", "LICENSE.txt", "LICENSE.md", "LICENCE", "COPYING"} {
				raw, err := readBounded(filepath.Join(root, name), 1<<20)
				if err != nil {
					continue
				}
				coverage := licensecheck.Scan(raw)
				if coverage.Percent >= 90 && len(coverage.Match) > 0 {
					ids := []string{}
					for _, match := range coverage.Match {
						if match.ID != "" && !contains(ids, match.ID) {
							ids = append(ids, match.ID)
						}
					}
					sort.Strings(ids)
					expr := strings.Join(ids, " AND ")
					if valid, _ := spdxexp.ValidateLicenses([]string{expr}); valid {
						l.Expression = expr
						l.Status = "identified"
						l.Reason = "license text identified by the license classifier"
						break
					}
				}
			}
		}
		out = append(out, l)
	}
	return out, hashes
}

type cargoMetadata struct {
	Packages []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Version      string `json:"version"`
		Source       string `json:"source"`
		License      string `json:"license"`
		ManifestPath string `json:"manifest_path"`
		Dependencies []struct {
			Name     string `json:"name"`
			Rename   string `json:"rename"`
			Kind     string `json:"kind"`
			Optional bool   `json:"optional"`
		} `json:"dependencies"`
	} `json:"packages"`
	Resolve struct {
		Root  string `json:"root"`
		Nodes []struct {
			ID   string `json:"id"`
			Deps []struct {
				Pkg   string `json:"pkg"`
				Name  string `json:"name"`
				Kinds []struct {
					Kind string `json:"kind"`
				} `json:"dep_kinds"`
			} `json:"deps"`
		} `json:"nodes"`
	} `json:"resolve"`
	WorkspaceMembers []string `json:"workspace_members"`
}

func rustInventory(ctx context.Context, dir string, args []string) (*security.Inventory, []LicenseFinding, error) {
	selected, err := cargo.SelectFor(ctx, dir, args)
	if err != nil {
		return nil, nil, err
	}
	cmdArgs := []string{"metadata", "--format-version", "1", "--locked", "--offline"}
	if target := cargo.TargetTriple(args); target != "" {
		cmdArgs = append(cmdArgs, "--filter-platform", target)
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--all-features" || arg == "--no-default-features" {
			cmdArgs = append(cmdArgs, arg)
		}
		if arg == "--features" || arg == "-F" {
			if i+1 >= len(args) {
				return nil, nil, errors.New("feature argument is missing")
			}
			i++
			cmdArgs = append(cmdArgs, arg, args[i])
		}
		if strings.HasPrefix(arg, "--features=") {
			cmdArgs = append(cmdArgs, arg)
		}
	}
	cmd := exec.CommandContext(ctx, "cargo", cmdArgs...)
	cmd.Dir = dir
	raw, err := cmd.Output()
	if err != nil {
		return nil, nil, errors.New("locked Cargo dependency metadata is unavailable; run cargo fetch and generate Cargo.lock before building")
	}
	if len(raw) > MaxDocumentBytes {
		return nil, nil, errors.New("Cargo metadata exceeds size limit")
	}
	var meta cargoMetadata
	if err = json.Unmarshal(raw, &meta); err != nil {
		return nil, nil, err
	}
	rootID := selected.Package.ID
	keys := map[string]string{}
	nodes := map[string][]string{}
	direct := map[string]bool{}
	for _, p := range meta.Packages {
		keys[p.ID] = security.ModuleKey(p.Name, p.Version)
	}
	for _, node := range meta.Resolve.Nodes {
		for _, d := range node.Deps {
			relevant := false
			for _, kind := range d.Kinds {
				if kind.Kind != "dev" {
					relevant = true
				}
			}
			if len(d.Kinds) == 0 {
				relevant = true
			}
			if relevant {
				nodes[node.ID] = append(nodes[node.ID], d.Pkg)
				if node.ID == rootID {
					direct[d.Pkg] = true
				}
			}
		}
	}
	reached := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if reached[id] {
			return
		}
		reached[id] = true
		for _, dep := range nodes[id] {
			walk(dep)
		}
	}
	walk(rootID)
	inv := &security.Inventory{Ecosystem: security.EcosystemRust, Level: security.InventoryResolved, Roots: []string{keys[rootID]}, Modules: []security.Module{}, Edges: []security.GraphEdge{}}
	if keys[rootID] == "" {
		return nil, nil, errors.New("selected Cargo package is absent from resolved graph")
	}
	licenses := []LicenseFinding{}
	for _, p := range meta.Packages {
		if !reached[p.ID] || p.ID == rootID {
			continue
		}
		sourceKind := security.SourcePath
		if strings.HasPrefix(p.Source, "registry+") {
			sourceKind = security.SourceRegistry
		}
		if strings.HasPrefix(p.Source, "git+") {
			sourceKind = security.SourceGit
		}
		matchable := p.Source == "registry+https://github.com/rust-lang/crates.io-index" || p.Source == "registry+sparse+https://index.crates.io/"
		if sourceKind == security.SourceRegistry && !matchable {
			sourceKind = security.SourceOtherRegistry
		}
		inv.Modules = append(inv.Modules, security.Module{Name: p.Name, Key: keys[p.ID], Version: p.Version, Direct: direct[p.ID], Resolved: true, ResolvedFrom: "cargo metadata --locked", Confidence: security.ConfidenceResolved, SourceKind: sourceKind, SourceDetail: safeRepository(p.Source), Matchable: matchable, Reachable: true})
		l := LicenseFinding{Package: p.Name, Version: p.Version, Status: "unknown", Reason: "Cargo license metadata is missing; manual review is required"}
		expr := strings.ReplaceAll(p.License, "/", " OR ")
		normalized, invalid := spdxexp.ValidateAndNormalizeLicensesWithOptions([]string{expr}, spdxexp.ValidateLicensesOptions{})
		if len(invalid) == 0 && len(normalized) == 1 {
			l.Expression = normalized[0]
			l.Status = "identified"
			l.Reason = "SPDX license expression declared in Cargo metadata"
		}
		licenses = append(licenses, l)
	}
	for from, deps := range nodes {
		if reached[from] {
			for _, to := range deps {
				if reached[to] {
					inv.Edges = append(inv.Edges, security.GraphEdge{From: keys[from], To: keys[to]})
				}
			}
		}
	}
	sort.Slice(inv.Modules, func(i, j int) bool { return inv.Modules[i].Key < inv.Modules[j].Key })
	sort.Slice(inv.Edges, func(i, j int) bool {
		if inv.Edges[i].From == inv.Edges[j].From {
			return inv.Edges[i].To < inv.Edges[j].To
		}
		return inv.Edges[i].From < inv.Edges[j].From
	})
	return inv, licenses, nil
}
