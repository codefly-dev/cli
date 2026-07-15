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

// ModuleCmd represents the run command
var ModuleCmd = &cobra.Command{
	Use:   "module",
	Short: "Open a module in your editor",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		provider := wool.New(ctx, resources.CLI.AsResource())

		provider.WithLogger(cli.GetLogger())
		defer provider.Done()

		ctx = provider.Inject(ctx)
		module, err := common.LoadModule(ctx)
		if err != nil {
			return fmt.Errorf("cannot load module: %w", err)
		}
		if err := exec.CommandContext(ctx, editor, module.Dir()).Run(); err != nil {
			return fmt.Errorf("cannot open module: %w", err)
		}
		return nil
	},
}

func init() {
	ModuleCmd.Flags().StringVar(&editor, "editor", "code", "your editor: 'code' for vscode, 'goland' for goland")
}
