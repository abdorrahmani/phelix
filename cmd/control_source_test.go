package cmd

import (
	"path/filepath"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	"github.com/abdorrahmani/phelix/internal/controller"
	"github.com/spf13/cobra"
)

func TestNamedDeploymentUsesRegisteredMountedSource(t *testing.T) {
	rootPath := t.TempDir()
	withJobManager(t, map[string]*app.AppInfo{"app": {ID: "app", Name: "sample", Directory: rootPath}})
	root := &cobra.Command{Use: "phelix"}
	root.AddCommand(&cobra.Command{Use: "deploy"})
	cfg := controller.Config{Mode: "container", Sources: []controller.SourceMount{{Host: "/host/projects", Controller: rootPath}}}
	request := &pb.CommandRequest{Argv: []string{"deploy", "sample"}, Directory: "/unmounted/home"}
	if _, err := validateControlCommand(root, cfg, request); err != nil || request.Directory != rootPath {
		t.Fatal(request, err)
	}
	request = &pb.CommandRequest{Argv: []string{"deploy", "sample", "--source-dir=."}, Directory: "/host/projects"}
	if _, err := validateControlCommand(root, cfg, request); err != nil || request.Argv[2] != "--source-dir="+rootPath {
		t.Fatal("relative project source was not mapped", request, err)
	}

	request = &pb.CommandRequest{Argv: []string{"deploy", "unknown"}, Directory: "/unmounted/home"}
	if _, err := validateControlCommand(root, cfg, request); err == nil {
		t.Fatal("unmounted source was accepted")
	}
	request = &pb.CommandRequest{Argv: []string{"deploy", "sample", "--source-dir=" + filepath.Dir(rootPath)}, Directory: rootPath}
	if _, err := validateControlCommand(root, cfg, request); err == nil {
		t.Fatal("unmounted source override was accepted")
	}
}
