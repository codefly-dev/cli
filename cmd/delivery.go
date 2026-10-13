package cmd

import (
	"github.com/codefly-dev/cli/cmd/delivery"
	"github.com/spf13/cobra"
)

var DeliveryCmd = &cobra.Command{Use: "delivery", Short: "Manage delivery signing keys", Long: "Manage the local keys used to sign delivery documents for a local cluster.", Example: "  codefly delivery key"}

func init() { DeliveryCmd.AddCommand(delivery.KeyCmd) }
