package workspace

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func goModuleWithMain(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/"+filepath.Base(dir)+"\n\ngo 1.27\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
}

func TestParseGoWork_Table(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
		wantErr bool
	}{
		{
			"block form",
			"go 1.27\n\nuse (\n\t./services/auth\n\t./services/orders\n)\n",
			[]string{"services/auth", "services/orders"},
			false,
		},
		{
			"single form",
			"go 1.27\n\nuse ./gateway\n",
			[]string{"gateway"},
			false,
		},
		{
			"quoted entries and comments",
			"go 1.27 // header comment\n\nuse (\n\t\"./services/auth\" // core auth\n\t./services/orders\n)\n",
			[]string{"services/auth", "services/orders"},
			false,
		},
		{
			"replace ignored",
			"use ./a\nreplace example.com/b => ../b\n",
			[]string{"a"},
			false,
		},
		{
			"unterminated block",
			"use (\n./a\n",
			nil,
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseGoWork([]byte(tc.content))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseGoWork() = %v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGoWork: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseGoWork() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDiscover_GoWork(t *testing.T) {
	root := t.TempDir()
	goModuleWithMain(t, filepath.Join(root, "services", "auth"))
	goModuleWithMain(t, filepath.Join(root, "services", "orders"))
	writeFile(t, filepath.Join(root, GoWorkFile), "go 1.27\n\nuse (\n\t./services/auth\n\t./services/orders\n)\n")

	d, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if d.Source != SourceGoWork {
		t.Fatalf("Source = %q, want go.work", d.Source)
	}
	if !reflect.DeepEqual(d.Members, []string{"services/auth", "services/orders"}) {
		t.Fatalf("Members = %v", d.Members)
	}
}

func TestDiscover_GoWorkMissingMemberFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, GoWorkFile), "go 1.27\n\nuse (\n\t./services/auth\n\t./services/ghost\n)\n")
	goModuleWithMain(t, filepath.Join(root, "services", "auth"))

	if _, err := Discover(root); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Discover() = %v, want missing-member error", err)
	}
}

func TestDiscover_CargoWorkspace(t *testing.T) {
	root := t.TempDir()
	writeCargoCrate(t, filepath.Join(root, "crates", "auth"), "auth")
	writeCargoCrate(t, filepath.Join(root, "crates", "orders"), "orders")
	writeFile(t, filepath.Join(root, "Cargo.toml"), `
[workspace]
members = ["crates/auth", "crates/orders"]
`)
	d, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if d.Source != SourceCargo {
		t.Fatalf("Source = %q, want cargo", d.Source)
	}
	if !reflect.DeepEqual(d.Members, []string{"crates/auth", "crates/orders"}) {
		t.Fatalf("Members = %v", d.Members)
	}
}

func TestDiscover_CargoGlobsExcludesAndDefaultMembers(t *testing.T) {
	root := t.TempDir()
	writeCargoCrate(t, filepath.Join(root, "crates", "auth"), "auth")
	writeCargoCrate(t, filepath.Join(root, "crates", "orders"), "orders")
	writeCargoCrate(t, filepath.Join(root, "crates", "shared-lib"), "shared_lib")
	writeFile(t, filepath.Join(root, "Cargo.toml"), `
[workspace]
members = ["crates/*", "gateway"]
exclude = ["crates/shared-lib"]
`)
	writeCargoCrate(t, filepath.Join(root, "gateway"), "gateway")

	d, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !reflect.DeepEqual(d.Members, []string{"crates/auth", "crates/orders", "gateway"}) {
		t.Fatalf("Members = %v", d.Members)
	}
}

func TestDiscover_CargoDefaultMembersWin(t *testing.T) {
	root := t.TempDir()
	writeCargoCrate(t, filepath.Join(root, "crates", "auth"), "auth")
	writeCargoCrate(t, filepath.Join(root, "crates", "orders"), "orders")
	writeFile(t, filepath.Join(root, "Cargo.toml"), `
[workspace]
members = ["crates/*"]
default-members = ["crates/auth"]
`)
	d, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !reflect.DeepEqual(d.Members, []string{"crates/auth"}) {
		t.Fatalf("Members = %v, want only default-members", d.Members)
	}
}

func TestDiscover_NoWorkspaceFound(t *testing.T) {
	root := t.TempDir()
	_, err := Discover(root)
	if err == nil {
		t.Fatalf("Discover on empty dir should fail")
	}
}

func TestDiscoverRunnables_FiltersLibraries(t *testing.T) {
	root := t.TempDir()
	goModuleWithMain(t, filepath.Join(root, "services", "auth"))
	writeFile(t, filepath.Join(root, "services", "types", "go.mod"), "module example.com/types\n\ngo 1.27\n")
	writeCargoCrate(t, filepath.Join(root, "crates", "orders"), "orders")
	writeFile(t, filepath.Join(root, "crates", "shared", "Cargo.toml"), "[package]\nname = \"shared\"\nversion = \"0.1.0\"\n")
	writeFile(t, filepath.Join(root, "crates", "shared", "src", "lib.rs"), "pub fn f() {}\n")

	members := []string{"services/auth", "services/types", "crates/orders", "crates/shared"}
	runnable, err := DiscoverRunnables(root, members)
	if err != nil {
		t.Fatalf("DiscoverRunnables: %v", err)
	}
	if len(runnable) != 2 {
		t.Fatalf("runnable = %v, want auth and orders", runnable)
	}
	if runnable["auth"] != "services/auth" || runnable["orders"] != "crates/orders" {
		t.Fatalf("runnable = %v", runnable)
	}
}

func TestDiscoverRunnables_NameCollisionFails(t *testing.T) {
	root := t.TempDir()
	goModuleWithMain(t, filepath.Join(root, "a", "auth"))
	goModuleWithMain(t, filepath.Join(root, "b", "auth"))
	_, err := DiscoverRunnables(root, []string{"a/auth", "b/auth"})
	if err == nil || !strings.Contains(err.Error(), "both resolve to service name") {
		t.Fatalf("DiscoverRunnables() = %v, want collision error", err)
	}
}

func writeCargoCrate(t *testing.T, dir, name string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname = \""+name+"\"\nversion = \"0.1.0\"\n")
	writeFile(t, filepath.Join(dir, "src", "main.rs"), "fn main() {}\n")
}
