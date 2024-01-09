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

// ApplicationCmd represents the run command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Open a application in your editor",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()

		provider := wool.New(ctx, configurations.CLI.AsResource())

		provider.WithLogger(cli.GetLogger())
		defer provider.Done()

		ctx = provider.Inject(ctx)
		application := common.Application(ctx)
		c := exec.Command(editor, application.Dir())
		err := c.Run()
		cli.ExitOnError(err, "cannot open application")
	},
}

func init() {
	ApplicationCmd.Flags().StringVar(&editor, "editor", "code", "your editor: 'code' for vscode, 'goland' for goland")
}
