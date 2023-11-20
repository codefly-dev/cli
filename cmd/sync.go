package cmd

import (
	"github.com/codefly-dev/cli/cmd/sync"
	"github.com/spf13/cobra"
)

// SyncCmd represents the Sync command
var SyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync (auto-generation, auto-linting)",
}

func init() {
	SyncCmd.AddCommand(sync.PartialCmd)
	SyncCmd.AddCommand(sync.ApplicationCmd)
	SyncCmd.AddCommand(sync.ServiceCmd)
}
