package cmd

import (
	"github.com/codefly-dev/cli/cmd/sync"
	"github.com/spf13/cobra"
)

// SyncCmd represents the Sync command
var SyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Reconcile services, libraries, or composed modules with their dependencies",
}

func init() {
	SyncCmd.AddCommand(sync.ServiceCmd)
	SyncCmd.AddCommand(sync.LibraryDependenciesCmd)
	SyncCmd.AddCommand(sync.ModuleCmd)
}
