package cmd

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/platform"
	"github.com/spf13/cobra"
)

// LoginCmd represents the login command
var LoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate this workspace with the Codefly platform",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return fmt.Errorf("cannot load workspace: %w", err)
		}

		token, err := platform.LoadToken(ctx, workspace)
		if err != nil {
			return fmt.Errorf("cannot load login token: %w", err)
		}

		if err = platform.Login(ctx, token); err != nil {
			return fmt.Errorf("cannot login: %w", err)
		}
		return nil
	},
}
