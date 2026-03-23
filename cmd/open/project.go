package open

import (
	"context"
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
	Short: "Open a workspace in your editor",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()

		provider := wool.New(ctx, resources.CLI.AsResource())

		provider.WithLogger(cli.GetLogger())
		defer provider.Done()

		ctx = provider.Inject(ctx)
		workspace := common.Workspace(ctx)
		c := exec.Command(editor, workspace.Dir())
		err := c.Run()
		cli.ExitOnError(err, "cannot open workspace")

	},
}

func init() {
	WorkspaceCmd.Flags().StringVar(&editor, "editor", "code", "your editor: 'code' for vscode, 'goland' for goland")
}
