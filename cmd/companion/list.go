package companion

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// ListCmd prints every companion discovered under <core>/companions/.
// Useful for confirming the CLI sees what you expect before invoking
// `companion build --all` against the wrong tree.
var ListCmd = &cobra.Command{
	Use:   "list",
	Short: "List companion images discovered under <core>/companions/",
	RunE:  runList,
}

func init() {
	ListCmd.Flags().String("core-dir", "", "Path to the core directory (default: walk up from cwd)")
}

func runList(cmd *cobra.Command, _ []string) error {
	coreDirFlag, _ := cmd.Flags().GetString("core-dir")
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cannot read working directory: %w", err)
	}
	coreDir := coreDirFlag
	if coreDir == "" {
		coreDir = FindCompanionsRoot(cwd)
	}

	cs, err := ListCompanions(coreDir)
	if err != nil {
		return fmt.Errorf("scan companions: %w", err)
	}
	if len(cs) == 0 {
		fmt.Printf("(no companions found under %s/companions/)\n", coreDir)
		return nil
	}
	for _, c := range cs {
		// Format: name@version  flake?docker?  dir
		flags := ""
		if c.HasFlake {
			flags += "flake "
		}
		if c.HasDockerfile {
			flags += "docker "
		}
		fmt.Printf("%-12s %-10s [%s]  %s\n", c.Name, c.Info.Version, flags, c.Dir)
	}
	return nil
}
