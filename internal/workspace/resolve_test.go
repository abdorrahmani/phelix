package workspace

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func seedMeshRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	goModuleWithMain(t, filepath.Join(root, "services", "auth"))
	goModuleWithMain(t, filepath.Join(root, "services", "orders"))
	goModuleWithMain(t, filepath.Join(root, "gateway"))
	return root
}

func TestResolve_FromManifest(t *testing.T) {
	root := seedMeshRepo(t)
	writeManifest(t, root, `
name: mesh
services:
  auth: { path: ./services/auth, port: 8081, strategy: blue-green }
  orders: { path: ./services/orders, port: 8082, depends_on: [auth] }
  gateway: { path: ./gateway, port: 8080, depends_on: [auth, orders] }
`)
	w, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if w.Name != "mesh" || w.Source != SourceManifest {
		t.Fatalf("workspace = %q / %q, want mesh / manifest", w.Name, w.Source)
	}
	if len(w.Services) != 3 {
		t.Fatalf("len(Services) = %d, want 3", len(w.Services))
	}
	gateway := w.Service("gateway")
	if gateway.Path != filepath.Join(root, "gateway") {
		t.Fatalf("gateway.Path = %q", gateway.Path)
	}
	order, err := w.TopoOrder()
	if err != nil {
		t.Fatalf("TopoOrder: %v", err)
	}
	if !reflect.DeepEqual(order, []string{"auth", "orders", "gateway"}) {
		t.Fatalf("order = %v", order)
	}
}

func TestResolve_FromGoWorkWhenNoManifest(t *testing.T) {
	root := seedMeshRepo(t)
	writeFile(t, filepath.Join(root, GoWorkFile), "go 1.27\n\nuse (\n\t./services/auth\n\t./services/orders\n\t./gateway\n)\n")
	w, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if w.Source != SourceGoWork {
		t.Fatalf("Source = %q, want go.work", w.Source)
	}
	if w.Name != filepath.Base(root) {
		t.Fatalf("Name = %q, want dir base", w.Name)
	}
	for _, svc := range w.Services {
		if svc.Port != 0 {
			t.Fatalf("discovered service %s should have no port yet, got %d", svc.Name, svc.Port)
		}
	}
}

func TestResolve_MissingServicePathFails(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "services:\n  auth: { path: ./services/auth }\n")
	_, err := Resolve(root)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Resolve() = %v, want missing-path error", err)
	}
}

func TestResolve_EscapingPathFails(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "services:\n  auth: { path: ../../outside }\n")
	_, err := Resolve(root)
	if err == nil || !strings.Contains(err.Error(), "escapes the workspace root") {
		t.Fatalf("Resolve() = %v, want escape error", err)
	}
}

func TestResolve_DuplicateExplicitPortFails(t *testing.T) {
	root := seedMeshRepo(t)
	writeManifest(t, root, "services:\n  auth: { path: ./services/auth, port: 8080 }\n  orders: { path: ./services/orders, port: 8080 }\n")
	_, err := Resolve(root)
	if err == nil || !strings.Contains(err.Error(), "duplicate port") {
		t.Fatalf("Resolve() = %v, want duplicate-port error", err)
	}
}

func TestResolve_CycleFails(t *testing.T) {
	root := seedMeshRepo(t)
	writeManifest(t, root, `
services:
  auth: { path: ./services/auth, depends_on: [orders] }
  orders: { path: ./services/orders, depends_on: [auth] }
`)
	_, err := Resolve(root)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("Resolve() = %v, want cycle error", err)
	}
}

func TestResolve_ManifestWithDiscoverMergesMembers(t *testing.T) {
	root := seedMeshRepo(t)
	writeManifest(t, root, `
name: mesh
discover: true
services:
  auth: { path: ./services/auth, port: 8081 }
`)
	writeFile(t, filepath.Join(root, GoWorkFile), "go 1.27\n\nuse (\n\t./services/auth\n\t./services/orders\n)\n")
	w, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(w.Services) != 2 {
		t.Fatalf("len(Services) = %d, want 2 (manifest auth + discovered orders)", len(w.Services))
	}
	if w.Service("auth").Port != 8081 {
		t.Fatalf("auth port = %d, want the manifest value", w.Service("auth").Port)
	}
	if w.Service("orders").Port != 0 {
		t.Fatalf("orders should stay unassigned, got %d", w.Service("orders").Port)
	}
}

func TestAssignPorts_SkipsUsedPorts(t *testing.T) {
	w := &Workspace{
		Name: "mesh",
		Services: []*Service{
			{Name: "auth", Port: 8081},
			{Name: "gateway"},
			{Name: "orders"},
			{Name: "billing"},
		},
	}
	if err := AssignPorts(w, 8080); err != nil {
		t.Fatalf("AssignPorts: %v", err)
	}
	if w.Service("billing").Port != 8080 {
		t.Fatalf("billing port = %d, want 8080 (name-sorted first)", w.Service("billing").Port)
	}
	if w.Service("gateway").Port != 8082 {
		t.Fatalf("gateway port = %d, want 8082 (8080 taken by billing, 8081 by auth)", w.Service("gateway").Port)
	}
	if w.Service("orders").Port != 8083 {
		t.Fatalf("orders port = %d, want 8083", w.Service("orders").Port)
	}
	if w.Service("auth").Port != 8081 {
		t.Fatalf("auth port must stay explicit, got %d", w.Service("auth").Port)
	}
}

func TestAssignPorts_InvalidBaseFails(t *testing.T) {
	w := &Workspace{Services: []*Service{{Name: "auth"}}}
	if err := AssignPorts(w, 99999); err == nil {
		t.Fatalf("AssignPorts with bad base should fail")
	}
}

func TestManifestExists(t *testing.T) {
	dir := t.TempDir()
	if ManifestExists(dir) {
		t.Fatalf("ManifestExists on empty dir = true")
	}
	writeManifest(t, dir, "services:\n  auth: { path: ./auth }\n")
	if !ManifestExists(dir) {
		t.Fatalf("ManifestExists after write = false")
	}
	if ManifestPath(dir) != filepath.Join(dir, FileName) {
		t.Fatalf("ManifestPath = %q", ManifestPath(dir))
	}
}
