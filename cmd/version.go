package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

const Version = "0.0.1"

var VersionCmd = &cobra.Command{
	Use:   "version",
	Short: "output the version number",
	Long:  `All software has versions. This is Phelix's`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println(Version)
		return nil
	},
}
