package cmd

import (
	"github.com/codefly-dev/cli/cmd/upgrade"
	"github.com/spf13/cobra"
)

// UpgradeCmd is the parent for `codefly upgrade service` and
// `codefly upgrade workspace`.
var UpgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Apply semver-safe dependency bumps",
}

func init() {
	UpgradeCmd.AddCommand(upgrade.ServiceCmd)
	UpgradeCmd.AddCommand(upgrade.WorkspaceCmd)
}
