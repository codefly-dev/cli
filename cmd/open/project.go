package open

import (
	"context"
	"os/exec"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ProjectCmd represents the run command
var ProjectCmd = &cobra.Command{
	Use:   "project",
	Short: "Open a project in your editor",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()

		provider := wool.New(ctx, configurations.CLI.AsResource())

		provider.WithLogger(common.CLI())
		defer provider.Done()

		ctx = provider.Inject(ctx)
		project := common.Project(ctx)
		c := exec.Command(editor, project.Dir())
		err := c.Run()
		cli.ExitOnError(err, "cannot open project")

	},
}

func init() {
	ProjectCmd.Flags().StringVar(&editor, "editor", "code", "your editor: 'code' for vscode, 'goland' for goland")
}
