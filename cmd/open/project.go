package open

import (
	"fmt"
	"os/exec"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// WorkspaceCmd represents the run command
var WorkspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Open the current workspace in your configured editor",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		provider := wool.New(ctx, resources.CLI.AsResource())

		provider.WithLogger(cli.GetLogger())
		defer provider.Done()

		ctx = provider.Inject(ctx)
		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return fmt.Errorf("cannot load workspace: %w", err)
		}
		if err := exec.CommandContext(ctx, editor, workspace.Dir()).Run(); err != nil {
			return fmt.Errorf("cannot open workspace: %w", err)
		}
		return nil
	},
}

func init() {
	WorkspaceCmd.Flags().StringVar(&editor, "editor", "code", "your editor: 'code' for vscode, 'goland' for goland")
}
