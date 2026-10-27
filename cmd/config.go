package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// configLintResult is the machine-contract result of `phelix config lint`.
// A failure keeps this shape out of the document: the boundary's error
// envelope already carries the code and message.
type configLintResult struct {
	Path  string `json:"path"`
	Valid bool   `json:"valid"`
}

var configLintJSON bool

var ConfigCmd = &cobra.Command{
	Use:   "config",
	Short: "Check phelix.yaml without running a command",
	Long: "Validates the project configuration of the current directory with the same loader every " +
		"command uses. `phelix config schema` prints the JSON Schema of phelix.yaml for editor " +
		"completion and hover documentation.",
}

var configLintCmd = &cobra.Command{
	Use:   "lint",
	Short: "Validate phelix.yaml and report the result",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if configLintJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		dir, err := currentDir()
		if err != nil {
			return err
		}
		path := filepath.Join(dir, project.FileName)

		cfg, lintErr := project.Load(dir)
		if lintErr != nil {
			return lintErr
		}
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", configLintResult{Path: path, Valid: true}))
		}
		fmt.Printf("  %s %s is valid (%d health endpoint(s), strategy %s)\n",
			color.GreenString("✓"), color.CyanString(path),
			healthEndpointCount(cfg), color.CyanString(deployStrategyLabel(cfg)))
		return nil
	},
}

var configSchemaCmd = &cobra.Command{
	Use:   "schema",
	Short: "Print the JSON Schema of phelix.yaml for editor completion",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		raw, err := project.SchemaJSON()
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(raw)
		return err
	},
}

// deployStrategyLabel reports the strategy the config would resolve to, or
// the classic fallback when the file declares none.
func deployStrategyLabel(cfg *project.Config) string {
	if cfg.Deploy != nil && cfg.Deploy.Strategy != "" {
		return cfg.Deploy.Strategy
	}
	return project.StrategyClassic
}

// healthEndpointCount reports how many health endpoints the file declares.
func healthEndpointCount(cfg *project.Config) int {
	if cfg.Health == nil {
		return 0
	}
	return len(cfg.Health.Endpoints)
}

func init() {
	ConfigCmd.AddCommand(configLintCmd, configSchemaCmd)
	configLintCmd.Flags().BoolVar(&configLintJSON, "json", false, "Output the machine envelope on stdout")
}
