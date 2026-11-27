package cmd

import (
	"errors"
	"fmt"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/spf13/cobra"
)

func init() {
	var preview, remove bool
	command := &cobra.Command{
		Use: "network ID", Short: "Preview or remove one unused Codefly-owned Docker network",
		Long: "Select an orphaned network by its full lowercase Docker ID. Inspect its ownership and attachments with --preview, then use --remove to repeat those checks and remove that exact network. Networks referenced by stopped containers are preserved.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if preview == remove {
				return errors.New("select exactly one of --preview or --remove")
			}
			ok, err := orchestration.RecoverUnusedNetwork(cmd.Context(), args[0], remove)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("network is missing, has no matching Codefly ownership labels, or is referenced by a container")
			}
			if preview {
				fmt.Fprintln(cmd.OutOrStdout(), "unused Codefly network eligible for removal:", args[0])
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "removed network:", args[0])
			}
			return nil
		},
	}
	command.Flags().BoolVar(&preview, "preview", false, "Check ownership and attachments without removing the network")
	command.Flags().BoolVar(&remove, "remove", false, "Remove only this exact network after checking ownership and attachments")
	RecoverCmd.AddCommand(command)
}
