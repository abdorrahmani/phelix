package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestControllerExamplesAreStandaloneAndExcludePrivateFiles(t *testing.T) {
	output := t.TempDir()
	command := exec.Command("bash", ".github/scripts/build-examples.sh", "1.3.0-beta.1", output)
	if log, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, log)
	}
	for _, name := range []string{"docker-host-controller", "docker-container-controller"} {
		file, err := os.Open(filepath.Join(output, name+".tar.gz"))
		if err != nil {
			t.Fatal(err)
		}
		reader, err := gzip.NewReader(file)
		if err != nil {
			file.Close()
			t.Fatal(err)
		}
		archive := tar.NewReader(reader)
		files := map[string]string{}
		for {
			header, err := archive.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if header.Typeflag != tar.TypeReg {
				continue
			}
			data, err := io.ReadAll(archive)
			if err != nil {
				t.Fatal(err)
			}
			files[strings.TrimPrefix(header.Name, name+"/")] = string(data)
		}
		reader.Close()
		file.Close()
		for _, required := range []string{"README.md", "setup.sh", "compose.yaml", "VERSION", "app/Dockerfile", "app/.dockerignore", "app/go.mod", "app/main.go", "app/phelix.yaml"} {
			if files[required] == "" {
				t.Fatal("missing runnable example file", name, required)
			}
		}
		for _, private := range []string{".env", "controller.yaml", "compose.build.yaml", "state/agent-id"} {
			if _, ok := files[private]; ok {
				t.Fatal("private or checkout-only file shipped", private)
			}
		}
		if files["VERSION"] != "1.3.0-beta.1\n" {
			t.Fatal("example version mismatch")
		}
		if name == "docker-container-controller" && strings.Contains(files["compose.yaml"], "build:") {
			t.Fatal("published example requires a source checkout")
		}
	}
}
