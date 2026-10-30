package cmd

import (
	"github.com/codefly-dev/cli/cmd/delivery"
	"github.com/spf13/cobra"
)

var DeliveryCmd = &cobra.Command{Use: "delivery", Short: "Manage delivery signing keys and send prepared delivery documents", Long: "Manage the local keys used to sign delivery documents for a local cluster, and send the prepared paired documents a render wrote to the host's composition interface.", Example: "  codefly delivery key\n  codefly delivery apply --documents ./prepared --address localhost:8081 --identity-file ./token"}

func init() {
	DeliveryCmd.AddCommand(delivery.KeyCmd)
	DeliveryCmd.AddCommand(delivery.PrepareCmd)
	DeliveryCmd.AddCommand(delivery.ApplyCmd)
}
