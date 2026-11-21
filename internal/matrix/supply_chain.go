package matrix

import (
	"context"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/supplychain"
	"path/filepath"
)

func captureSecurity(ctx context.Context, root, name, language string, c Combination, args []string) (supplychain.BuildFacts, error) {
	output := filepath.Join(root, "builds", "matrix", c.ID(), c.BinaryName(name))
	facts := supplychain.BuildFacts{BuildID: c.ID(), AppID: name, AppName: name, Directory: root, Language: language, Arguments: args, OutputPath: output, TargetOS: c.OS, TargetArch: archWithVariant(c)}
	return supplychain.Capture(ctx, facts)
}

func finishSecurity(ctx context.Context, facts supplychain.BuildFacts, result *Result) {
	if result.Status != "success" || result.Artifact == "" {
		return
	}
	if _, err := supplychain.Generate(ctx, supplychain.NewStore(server.DataDir()), facts, result.Artifact); err != nil {
		result.Status = "failed"
		result.Error = err
	}
}
