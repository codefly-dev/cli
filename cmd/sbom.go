package cmd

import (
	sbomcmd "github.com/codefly-dev/cli/cmd/sbom"
	"github.com/spf13/cobra"
)

// SBOMCmd exposes service/workspace CycloneDX generation through each service
// agent's typed Builder.SBOM RPC.
var SBOMCmd = &cobra.Command{
	Use:   "sbom",
	Short: "Generate authoritative CycloneDX software bills of materials",
}

func init() {
	SBOMCmd.AddCommand(sbomcmd.ServiceCmd)
	SBOMCmd.AddCommand(sbomcmd.WorkspaceCmd)
}
