package supplychain

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/version"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type BuildFacts struct {
	BuildID         string
	AppID           string
	AppName         string
	Directory       string
	Language        string
	Arguments       []string
	OutputPath      string
	TargetOS        string
	TargetArch      string
	CompilerVersion string
	StartedAt       time.Time
	Source          Source
	Inputs          string
}

func Capture(ctx context.Context, f BuildFacts) (BuildFacts, error) {
	f.StartedAt = time.Now().UTC()
	f.Source = sourceInfo(ctx, f.Directory)
	digest, err := InputDigest(f.Directory, f.OutputPath)
	if err != nil {
		return f, err
	}
	f.Inputs = digest
	f.Source.SHA256 = digest
	return f, nil
}

func InputDigest(root, output string) (string, error) {
	h := sha256.New()
	count := 0
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if path != root && (e.Name() == ".git" || e.Name() == "target" || e.Name() == "builds" || e.Name() == "node_modules" || strings.HasPrefix(e.Name(), ".phelix")) {
				return filepath.SkipDir
			}
			return nil
		}
		if path == output || strings.HasSuffix(path, ".supply-chain.json") || strings.HasSuffix(path, ".log") || (strings.HasPrefix(e.Name(), "app_") && filepath.Ext(e.Name()) == "") {
			return nil
		}
		if e.Type()&fs.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, resolved)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return errors.New("build input symlink escapes source directory")
			}
		}

		count++
		if count > 100000 {
			return errors.New("source inventory exceeds file limit")
		}
		digest, size, err := SHA256File(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00%s\n", rel, size, digest)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sourceInfo(ctx context.Context, dir string) Source {
	run := func(args ...string) string {
		c := exec.CommandContext(ctx, "git", args...)
		c.Dir = dir
		raw, err := c.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(raw))
	}
	commit := run("rev-parse", "HEAD")
	if len(commit) != 40 && len(commit) != 64 {
		commit = ""
	}
	return Source{Repository: safeRepository(run("config", "--get", "remote.origin.url")), Commit: commit, Ref: run("symbolic-ref", "--short", "-q", "HEAD"), Dirty: run("status", "--porcelain", "--untracked-files=normal") != ""}
}

func safeRepository(value string) string {
	u, err := url.Parse(value)
	if err == nil && u.Scheme != "" {
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		return u.String()
	}
	if strings.Contains(value, "@") {
		value = value[strings.LastIndex(value, "@")+1:]
	}
	if strings.ContainsAny(value, "\n\r\x00") || strings.Contains(value, "?") {
		return ""
	}
	return value
}

func SafeArguments(args []string) []string {
	var out []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			name := strings.SplitN(arg, "=", 2)[0]
			if len(name) < 80 && !strings.ContainsAny(name, "\x00\n\r\t \"'") {
				out = append(out, name)
			}
		}
	}
	return out
}

func Generate(ctx context.Context, s *Store, f BuildFacts, path string) (*Report, error) {
	digest, err := InputDigest(f.Directory, f.OutputPath)
	if err != nil {
		return nil, err
	}
	if f.Inputs != "" && digest != f.Inputs {
		return nil, errors.New("build source or dependency files changed during compilation; retry with a stable source tree")
	}
	if f.Source.Commit != "" && sourceInfo(ctx, f.Directory).Commit != f.Source.Commit {
		return nil, errors.New("source revision changed during compilation")
	}
	hash, size, err := SHA256File(path)
	if err != nil {
		return nil, err
	}
	appID := f.AppID
	if appID == "" {
		appID = f.AppName
	}
	if appID == "" {
		appID = "unregistered"
	}
	name := f.AppName
	if name == "" {
		name = "unregistered"
	}
	p, err := s.LoadPolicy(appID)
	if err != nil {
		return nil, err
	}
	osName, arch := f.TargetOS, f.TargetArch
	if osName == "" {
		osName = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	inv, licenses, hashes, err := BuildInventory(ctx, f.Directory, f.Language, f.Arguments)
	if err != nil {
		return nil, err
	}
	compiler := f.CompilerVersion
	if f.Language == "go" {
		info, err := buildinfo.ReadFile(path)
		if err == nil {
			compiler = info.GoVersion
			linked := map[string]bool{}
			selected := map[string]bool{}
			for _, m := range inv.Modules {
				selected[m.Key] = true
			}
			for _, d := range info.Deps {
				if d.Replace != nil {
					d = d.Replace
					if d.Version == "(devel)" {
						copy := *d
						copy.Version = ""
						d = &copy
					}
				}
				key := security.ModuleKey(d.Path, d.Version)
				if !selected[key] {
					return nil, fmt.Errorf("artifact module %s differs from resolved dependency graph", key)
				}
				linked[key] = true
			}
			for i := range inv.Modules {
				inv.Modules[i].Reachable = linked[inv.Modules[i].Key]
			}
			for _, setting := range info.Settings {
				if setting.Key == "GOOS" {
					osName = setting.Value
				}
				if setting.Key == "GOARCH" {
					arch = setting.Value
				}
			}
		} else {
			inv.Level = security.InventoryDegraded
			inv.Notes = append(inv.Notes, "Go artifact build metadata is unavailable; dependency information cannot be cross-checked against the executable")
		}

	}
	if compiler == "" {
		tool := f.Language
		args := []string{"version"}
		if f.Language == "rust" {
			tool = "rustc"
			args = []string{"--version"}
			if len(f.Arguments) > 0 && strings.HasPrefix(f.Arguments[0], "+") {
				args = append([]string{f.Arguments[0]}, args...)
			}
		}
		c := exec.CommandContext(ctx, tool, args...)
		c.Dir = f.Directory
		if raw, err := c.Output(); err == nil {
			compiler = strings.TrimSpace(string(raw))
		}
	}
	compilerName := "go"
	if f.Language == "rust" {
		compilerName = "rustc"
	}
	r := &Report{BuildID: f.BuildID, PackageHashes: hashes, SchemaVersion: 1, AppID: appID, AppName: name, BuiltAt: time.Now().UTC(), Artifact: Artifact{Filename: filepath.Base(path), SHA256: hash, SizeBytes: size, TargetOS: osName, TargetArch: arch}, Source: f.Source, Builder: Builder{ID: "https://github.com/abdorrahmani/phelix", PhelixVersion: version.Version, Language: f.Language, Compiler: compilerName, CompilerVersion: compiler, Arguments: SafeArguments(f.Arguments)}, Inventory: inv, Licenses: licenses, Policy: p, Signature: Signature{Status: "missing", Reason: "artifact has not been signed"}}
	knownHashes := map[string]bool{}
	for _, m := range inv.Modules {
		knownHashes[m.Key] = true
	}
	for key := range r.PackageHashes {
		if !knownHashes[key] {
			delete(r.PackageHashes, key)
		}
	}

	if err := GenerateDocuments(s, r); err != nil {
		return nil, err
	}
	buildPolicy := p
	buildPolicy.Mode = "audit"
	r.Decisions, err = Evaluate(r, buildPolicy)
	if err != nil {
		return nil, err
	}
	if err = SaveSidecar(path, r); err != nil {
		return nil, err
	}
	return r, nil
}

func SaveSidecar(path string, r *Report) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomicWrite(path+".supply-chain.json", raw)
}

func LoadSidecar(path string) (*Report, error) {
	raw, err := readBounded(path+".supply-chain.json", MaxReportBytes)
	if err != nil {
		return nil, err
	}
	var r Report
	if err = json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if err = ValidateReport(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

func Finalize(ctx context.Context, s *Store, r *Report, path string, versionNumber int) (*Report, error) {
	hash, size, err := SHA256File(path)
	if err != nil {
		return nil, err
	}
	if hash != r.Artifact.SHA256 || size != r.Artifact.SizeBytes {
		return nil, errors.New("artifact changed before version persistence")
	}
	copy := *r
	copy.Version = versionNumber
	copy.Artifact.Filename = filepath.Base(path)
	copy.Signature = Signature{Status: "missing", Reason: "versioned artifact documents have not been signed"}
	if err = GenerateDocuments(s, &copy); err != nil {
		return nil, err
	}
	p, err := s.LoadPolicy(copy.AppID)
	if err != nil {
		return nil, err
	}
	copy.Policy = p
	if p.RequireScan || p.Mode != "disabled" {
		if err := rescan(ctx, s, &copy, false); err != nil {
			copy.Decisions = append(copy.Decisions, Decision{Rule: "scan", Status: "unknown", Reason: "vulnerability scanner unavailable"})
		}
	}
	if key := os.Getenv("PHELIX_SIGNING_KEY"); key != "" || os.Getenv("PHELIX_KEYLESS_SIGNING") == "1" {
		if err := sign(ctx, s, &copy, path, key, false); err != nil {
			copy.Signature.Reason = "automatic signing failed; inspect signing credentials and cosign availability"
		}
	}
	copy.Decisions, err = Evaluate(&copy, p)
	if err != nil {
		return nil, err
	}
	if err = SaveSidecar(path, &copy); err != nil {
		return nil, err
	}
	return &copy, nil
}

func Reference(r *Report) string { return reportKey(r) }

func sortedModules(inv *security.Inventory) []security.Module {
	mods := append([]security.Module{}, inv.Modules...)
	sort.Slice(mods, func(i, j int) bool { return mods[i].Key < mods[j].Key })
	return mods
}
