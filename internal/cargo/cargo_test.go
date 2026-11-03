package cargo

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMessageFormatArgsReplacesUserFormat(t *testing.T) {
	args := MessageFormatArgs([]string{"build", "--release", "--message-format", "short", "--message-format=json"})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "short") || strings.Contains(joined, "--message-format=json ") {
		t.Fatalf("user message format was not replaced: %s", joined)
	}
	if !strings.HasSuffix(joined, "--message-format=json-render-diagnostics") {
		t.Fatalf("missing machine-readable message format: %s", joined)
	}
}

func TestProfileDirAndTargetTriple(t *testing.T) {
	cases := []struct {
		args []string
		dir  string
	}{
		{nil, "debug"},
		{[]string{"--release"}, "release"},
		{[]string{"--profile", "production"}, "production"},
		{[]string{"--profile=dev"}, "debug"},
		{[]string{"--profile", "bench"}, "release"},
	}
	for _, tc := range cases {
		if got := ProfileDir(tc.args); got != tc.dir {
			t.Errorf("ProfileDir(%v) = %q, want %q", tc.args, got, tc.dir)
		}
	}
	if got := TargetTriple([]string{"--target", "aarch64-unknown-linux-gnu"}); got != "aarch64-unknown-linux-gnu" {
		t.Fatalf("TargetTriple = %q", got)
	}
}

func TestSelectWorkspaceMemberAndRenamedBin(t *testing.T) {
	root := t.TempDir()
	member := filepath.Join(root, "member")
	meta := &Metadata{
		WorkspaceRoot:    root,
		WorkspaceMembers: []string{"pkg-member", "pkg-other"},
		Packages: []Package{
			{
				ID: "pkg-member", Name: "lab", ManifestPath: filepath.Join(member, "Cargo.toml"),
				Targets: []Target{{Kind: []string{"bin"}, Name: "server-bin"}},
			},
			{
				ID: "pkg-other", Name: "other", ManifestPath: filepath.Join(root, "other", "Cargo.toml"),
				Targets: []Target{{Kind: []string{"bin"}, Name: "other"}},
			},
		},
	}

	sel, err := Select(meta, member, nil)
	if err != nil {
		t.Fatalf("Select member: %v", err)
	}
	if sel.Package.Name != "lab" || sel.Target.Name != "server-bin" {
		t.Fatalf("wrong selection: %+v", sel)
	}

	if _, err := Select(meta, root, nil); err == nil {
		t.Fatal("workspace root with several packages must be ambiguous")
	}

	sel, err = Select(meta, root, []string{"--package", "lab"})
	if err != nil || sel.Package.Name != "lab" {
		t.Fatalf("explicit package selection failed: %+v %v", sel, err)
	}
}

func TestSelectDefaultRunAndExplicitBin(t *testing.T) {
	root := t.TempDir()
	pkg := Package{
		ID: "pkg", Name: "multi", ManifestPath: filepath.Join(root, "Cargo.toml"),
		DefaultRun: "web",
		Targets: []Target{
			{Kind: []string{"bin"}, Name: "web"},
			{Kind: []string{"bin"}, Name: "worker"},
		},
	}
	meta := &Metadata{Packages: []Package{pkg}, WorkspaceRoot: root, WorkspaceMembers: []string{"pkg"}}

	sel, err := Select(meta, root, nil)
	if err != nil || sel.Target.Name != "web" {
		t.Fatalf("default-run selection failed: %+v %v", sel, err)
	}
	sel, err = Select(meta, root, []string{"--bin", "worker"})
	if err != nil || sel.Target.Name != "worker" {
		t.Fatalf("explicit --bin selection failed: %+v %v", sel, err)
	}
	pkg.DefaultRun = ""
	meta.Packages = []Package{pkg}
	if _, err := Select(meta, root, nil); err == nil {
		t.Fatal("several bins without default-run must be ambiguous")
	}
}

func TestSelectArtifactUsesExactPackageAndBin(t *testing.T) {
	selected := "/tmp/target/debug/app"
	other := "/tmp/target/debug/other"
	empty := ""
	artifacts := []Artifact{
		{Reason: "compiler-artifact", PackageID: "pkg", Target: Target{Kind: []string{"bin"}, Name: "app"}, Executable: &empty},
		{Reason: "compiler-artifact", PackageID: "pkg-other", Target: Target{Kind: []string{"bin"}, Name: "app"}, Executable: &other},
		{Reason: "compiler-artifact", PackageID: "pkg", Target: Target{Kind: []string{"bin"}, Name: "other"}, Executable: &other},
		{Reason: "compiler-artifact", PackageID: "pkg", Target: Target{Kind: []string{"bin"}, Name: "app"}, Executable: &selected, Fresh: true},
	}
	sel := Selection{Package: Package{ID: "pkg", Name: "lab"}, Target: Target{Name: "app"}}
	got, err := SelectArtifact(artifacts, sel)
	if err != nil {
		t.Fatalf("SelectArtifact: %v", err)
	}
	if got != selected {
		t.Fatalf("got %q, want %q", got, selected)
	}

	sel.Target.Name = "missing"
	if _, err := SelectArtifact(artifacts, sel); err == nil {
		t.Fatal("missing selected artifact must error")
	}
}

func TestParseArtifactsIgnoresNonJSON(t *testing.T) {
	stream := "Compiling app\n" +
		`{"reason":"compiler-artifact","package_id":"pkg","target":{"kind":["bin"],"name":"app"},"executable":"/tmp/app"}` + "\n" +
		`{"reason":"build-finished","success":true}` + "\n"
	artifacts, err := ParseArtifacts(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("ParseArtifacts: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0].Target.Name != "app" {
		t.Fatalf("unexpected artifacts: %+v", artifacts)
	}
}
