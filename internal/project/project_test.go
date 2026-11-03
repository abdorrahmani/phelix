package project

import (
	"os"
	"path/filepath"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{Name: "api", Port: 4000}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Name != "api" || got.Port != 4000 {
		t.Errorf("got %+v, want {api 4000}", got)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(t.TempDir())
	if code := phelixerr.CodeOf(err); code != phelixerr.CodeNotFound {
		t.Errorf("code = %q, want NOT_FOUND", code)
	}
}

func TestLoadMalformed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte("name: [unclosed\nport: notanint"), 0o644)
	if _, err := Load(dir); phelixerr.CodeOf(err) != phelixerr.CodeConfiguration {
		t.Errorf("malformed config code = %v, want CONFIGURATION_ERROR", phelixerr.CodeOf(err))
	}
}

func TestLoadInvalidPort(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte("name: api\nport: 70000"), 0o644)
	if _, err := Load(dir); err == nil {
		t.Error("port 70000 accepted, want error")
	}
}

func TestLoadPartialConfigNameOptional(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte("port: 3000"), 0o644)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("port-only config rejected: %v", err)
	}
	if cfg.Name != "" || cfg.Port != 3000 {
		t.Errorf("got %+v, want name empty and port 3000", cfg)
	}
}

func TestScanHardcodedPort(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(
		"package main\nimport \"net/http\"\nfunc main() {\n\thttp.ListenAndServe(\":3000\", nil)\n}\n"), 0o644)

	hits, readsPORT := ScanHardcodedPort(dir)
	if len(hits) != 1 || hits[0].Port != 3000 || hits[0].Line != 4 {
		t.Fatalf("hits = %+v, want one :3000 at line 4", hits)
	}
	if hits[0].ReadsPORT {
		t.Error("hit.ReadsPORT = true, want false")
	}
	if readsPORT {
		t.Error("readsPORT = true, want false")
	}
}

func TestScanPORTAware(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(
		"package main\nimport (\n\"net/http\"\n\"os\"\n)\nfunc main() {\n\tport := os.Getenv(\"PORT\")\n\thttp.ListenAndServe(\":\"+port, nil)\n}\n"), 0o644)

	hits, readsPORT := ScanHardcodedPort(dir)
	if len(hits) != 0 {
		t.Errorf("hits = %+v, want none (concatenated \":\"+port is not a literal)", hits)
	}
	if !readsPORT {
		t.Error("readsPORT = false, want true")
	}
}

func TestScanRustHardcoded(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.rs"), []byte(
		"fn main() {\n    let listener = TcpListener::bind(\"127.0.0.1:8080\").unwrap();\n}\n"), 0o644)

	hits, _ := ScanHardcodedPort(dir)
	if len(hits) != 1 || hits[0].Port != 8080 {
		t.Fatalf("hits = %+v, want one :8080", hits)
	}
}

func TestScanSkipsVendorAndGit(t *testing.T) {
	dir := t.TempDir()
	vendor := filepath.Join(dir, "vendor", "x")
	os.MkdirAll(vendor, 0o755)
	os.WriteFile(filepath.Join(vendor, "a.go"), []byte("http.ListenAndServe(\":9999\", nil)\n"), 0o644)
	git := filepath.Join(dir, ".git", "objects")
	os.MkdirAll(git, 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[core]\n"), 0o644)

	hits, _ := ScanHardcodedPort(dir)
	if len(hits) != 0 {
		t.Errorf("hits = %+v, want none from vendor/ or .git/", hits)
	}
}

func TestScanContinuesAfterIgnoredDirectories(t *testing.T) {
	dir := t.TempDir()
	for _, ignored := range []string{".git", "vendor", "node_modules", "target", "builds"} {
		nested := filepath.Join(dir, ignored, "nested")
		os.MkdirAll(nested, 0o755)
		os.WriteFile(filepath.Join(nested, "ignored.go"), []byte("http.ListenAndServe(\":9999\", nil)\n"), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(
		"package main\nfunc main() {\n\thttp.ListenAndServe(\":3000\", nil)\n}\n"), 0o644)

	hits, _ := ScanHardcodedPort(dir)
	if len(hits) != 1 || hits[0].Port != 3000 {
		t.Fatalf("hits = %+v, want the application listener after ignored dirs", hits)
	}
}

func TestScanRustPORTReads(t *testing.T) {
	tests := map[string]string{
		"qualified": "fn main() {\n    let port = std::env::var(\"PORT\").unwrap();\n}\n",
		"imported":  "use std::env;\nfn main() {\n    let port = env::var(\"PORT\").unwrap();\n}\n",
	}
	for name, src := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "main.rs"), []byte(src), 0o644)

			hits, readsPORT := ScanHardcodedPort(dir)
			if len(hits) != 0 {
				t.Errorf("hits = %+v, want none", hits)
			}
			if !readsPORT {
				t.Error("readsPORT = false, want true")
			}
		})
	}
}

func TestScanPORTAwarenessIsPerFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "listener.go"), []byte(
		"package main\nfunc main() {\n\thttp.ListenAndServe(\":3000\", nil)\n}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "config.go"), []byte(
		"package main\nimport \"os\"\nvar _ = os.Getenv(\"PORT\")\n"), 0o644)

	hits, readsPORT := ScanHardcodedPort(dir)
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want one listener", hits)
	}
	if !readsPORT {
		t.Error("readsPORT = false, want true for the project")
	}
	if hits[0].ReadsPORT {
		t.Error("hit.ReadsPORT = true, want false: another file reads PORT")
	}
}
