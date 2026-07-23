package cmd

import (
	"github.com/codefly-dev/cli/cmd/show"
	"github.com/spf13/cobra"
)

// ShowCmd groups read-only inspection of a workspace's configuration: the dependency
// graph, the network configuration (endpoints + addresses), and more. Everything under
// `show` reuses the SAME machinery `codefly run` uses (the architecture dependency graph,
// the network port hash) — so it reports exactly what a run will do, without starting it.
var ShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Inspect workspace dependency and network configuration",
}

func init() {
	ShowCmd.AddCommand(show.DependenciesCmd)
	ShowCmd.AddCommand(show.NetworkCmd)
}
