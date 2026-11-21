package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/deploy"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/supplychain"
)

func TestRemoteMatrixRejectsFailedSecurityFinalization(t *testing.T) {
	remoteMatrixEnv(t)
	t.Setenv("PHELIX_SIGNING_KEY", "")
	t.Setenv("PHELIX_KEYLESS_SIGNING", "")
	store := supplychain.NewStore(server.DataDir())
	p := supplychain.DefaultPolicy()
	p.Mode = "enforce"
	p.RequireSignature = true
	p.PublicKey = filepath.Join(t.TempDir(), "trusted.pub")
	if err := store.SavePolicy("app-1", p); err != nil {
		t.Fatal(err)
	}
	res := runRemote(buildReq("req-security-rejected", func(r *pb.MonitorCommandRequest) {
		r.Matrix.Platforms = []string{"linux/amd64"}
	}))
	if res.GetStatus() != "error" || !strings.Contains(res.GetError(), "security") {
		t.Fatalf("rejected version reported successful execution: %+v", res)
	}
	if res.GetMatrixRun().GetRelease() != nil {
		t.Fatal("rejected build published a release")
	}
	versions, err := deploy.LoadVersions("myapp")
	if err != nil || len(versions.Versions) != 0 {
		t.Fatalf("rejected build persisted a trusted version: %+v %v", versions, err)
	}
	if count, err := store.Count("app-1"); err != nil || count != 0 {
		t.Fatalf("rejected matrix build left accepted artifact evidence: %d %v", count, err)
	}
}
