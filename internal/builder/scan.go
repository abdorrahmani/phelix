package builder

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ignoredDirs is a set of directory names that should be ignored during scanning.
var ignoredDirs = map[string]bool{
	".git":         true,
	"vendor":       true,
	"node_modules": true,
	"target":       true,
}

// ProjectScan holds the results of scanning a project directory for Go files and packages.
type ProjectScan struct {
	Root   string
	Exists bool

	// mainFiles holds every file named main.go found outside ignoredDirs, in
	// deterministic walk order.
	mainFiles []string

	// hasMainPkg records that some .go file declares "package main".
	hasMainPkg bool

	// ignored holds directory names from the root .gitignore, merged over
	// ignoredDirs.
	ignored map[string]bool
}

// NewProjectScan scans the given root directory for Go files and packages, returning a ProjectScan struct with the results.
func NewProjectScan(root string) *ProjectScan {
	s := &ProjectScan{
		Root: root,
	}
	if _, err := os.Stat(root); err == nil {
		s.Exists = true
	}

	if !s.Exists {
		return s
	}

	s.ignored = loadGitignoreDirs(root)

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if d.IsDir() {
			if path != root && s.skipped(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}

		if d.Name() == "main.go" {
			s.mainFiles = append(s.mainFiles, path)
		}

		if !s.hasMainPkg && strings.HasSuffix(d.Name(), ".go") && declaresMainPackage(path) {
			s.hasMainPkg = true
		}
		return nil
	})
	return s
}

// declaresMainPackage checks if the given Go source file declares "package main".
func declaresMainPackage(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}

	defer func() { _ = f.Close() }()

	buf := make([]byte, 1024)
	n, _ := f.Read(buf)

	return n > 0 && strings.Contains(string(buf[:n]), "package main")
}

// RootFile checks if a file with the given name exists in the root directory of the project scan.
func (s *ProjectScan) RootFile(name string) bool {
	if s == nil || !s.Exists {
		return false
	}
	_, err := os.Stat(filepath.Join(s.Root, name))
	return err == nil
}

func (s *ProjectScan) HasMainFile() bool {
	return s != nil && len(s.mainFiles) > 0
}

// HasMainPackage returns true if any Go file in the project declares "package main".
func (s *ProjectScan) HasMainPackage() bool {
	return s != nil && s.hasMainPkg
}

// FirstMainFile returns the first main.go file found in walk order, or "" when there is
// none.
func (s *ProjectScan) FirstMainFile() string {
	if s == nil || len(s.mainFiles) == 0 {
		return ""
	}
	return s.mainFiles[0]
}

// loadGitignoreDirs reads the .gitignore file in the given root directory and returns a map of directory names that should be ignored during scanning.
func loadGitignoreDirs(root string) map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return out
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}

		line = strings.TrimSuffix(line, "/")
		if strings.ContainsAny(line, "*?[]\\/") {
			continue
		}

		out[line] = true
	}

	return out
}

// skipped reports whether a directory should be skipped.
func (s *ProjectScan) skipped(name string) bool {
	return ignoredDirs[name] || s.ignored[name]
}
