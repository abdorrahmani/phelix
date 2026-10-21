package phelixerr

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestEveryCodeHasAProducer(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	// Codes that legitimately have no producer, with the reason.
	allowed := map[string]string{
		"CodeOK":           "success sentinel; ExitCodeFor maps it, nothing errors with it",
		"CodeUnauthorized": "reserved for a backend 401; the CLI fails with UNAUTHENTICATED instead",
	}

	fset := token.NewFileSet()
	used := map[string]bool{}

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "releases", "node_modules", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "internal/errors/codes.go" || rel == "cmd/cli_errors.go" {
			return nil
		}
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && strings.HasPrefix(id.Name, "Code") {
				used[id.Name] = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}

	declared, err := declaredCodes(fset)
	if err != nil {
		t.Fatalf("parse codes.go: %v", err)
	}

	var orphans []string
	for name := range declared {
		if used[name] {
			continue
		}
		if _, ok := allowed[name]; ok {
			continue
		}
		orphans = append(orphans, name)
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		t.Fatalf("error codes declared but never produced: %s\n"+
			"Give each one a producer, delete it, or list it in allowed with a reason.",
			strings.Join(orphans, ", "))
	}

	// A stale allowlist entry is how this test quietly stops guarding anything.
	for name := range allowed {
		if !declared[name] {
			t.Errorf("allowed entry %q no longer exists in codes.go; remove it", name)
		}
	}
}

// declaredCodes returns every Code-prefixed constant declared in codes.go.
func declaredCodes(fset *token.FileSet) (map[string]bool, error) {
	f, err := parser.ParseFile(fset, "codes.go", nil, 0)
	if err != nil {
		return nil, err
	}
	declared := map[string]bool{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				if strings.HasPrefix(name.Name, "Code") {
					declared[name.Name] = true
				}
			}
		}
	}
	return declared, nil
}
