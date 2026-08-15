package cmd

import (
	"fmt"

	"github.com/abdorrahmani/phelix/internal/version"
	"github.com/spf13/cobra"
)

var (
	shortVersion   bool
	verboseVersion bool
)

var VersionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		if shortVersion {
			fmt.Println(version.Version)
			return
		}

		printVersion(verboseVersion)

		return
	},
}

func init() {
	VersionCmd.Flags().BoolVar(&shortVersion, "short", false, "Print only the version")
	VersionCmd.Flags().BoolVarP(&verboseVersion, "verbose", "v", false, "Print detailed build information")
}

func printVersion(verbose bool) {
	fmt.Printf("Phelix CLI %s\n\n", version.Version)

	fmt.Printf("%-12s%s\n", "Build ID", value(version.BuildID))
	fmt.Printf("%-12s%s\n", "Commit", value(version.Commit))
	fmt.Printf("%-12s%s\n", "Built", value(version.BuildTime))
	fmt.Printf("%-12s%s\n", "Platform", value(version.Platform()))

	if !verbose {
		return
	}

	fmt.Println()

	fmt.Printf("%-12s%s\n", "Compiler", version.Compiler())
	fmt.Printf("%-12s%s\n", "CGO", value(version.CGOEnabled))
}

func value(v string) string {
	if v == "" {
		return "-"
	}
	return v
}
