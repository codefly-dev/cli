package cmd

import (
	"github.com/codefly-dev/cli/cmd/get"
	"github.com/spf13/cobra"
)

// GetCmd groups read-only queries against the running daemon and the
// resource model.
var GetCmd = &cobra.Command{
	Use:   "get",
	Short: "Query services and endpoints",
}

func init() {
	GetCmd.AddCommand(get.EndpointsCmd)
}
