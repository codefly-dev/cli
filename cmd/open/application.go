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

// ModuleCmd represents the run command
var ModuleCmd = &cobra.Command{
	Use:   "module",
	Short: "Open a module in your editor",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()

		provider := wool.New(ctx, resources.CLI.AsResource())

		provider.WithLogger(cli.GetLogger())
		defer provider.Done()

		ctx = provider.Inject(ctx)
		module := common.Module(ctx)
		c := exec.Command(editor, module.Dir())
		err := c.Run()
		cli.ExitOnError(err, "cannot open module")
	},
}

func init() {
	ModuleCmd.Flags().StringVar(&editor, "editor", "code", "your editor: 'code' for vscode, 'goland' for goland")
}
