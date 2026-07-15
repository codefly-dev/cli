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

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Open a service in your editor",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		provider := wool.New(ctx, resources.CLI.AsResource())

		provider.WithLogger(cli.GetLogger())
		defer provider.Done()

		ctx = provider.Inject(ctx)
		service, err := common.LoadService(ctx)
		if err != nil {
			return fmt.Errorf("cannot load service: %w", err)
		}
		if err := exec.CommandContext(ctx, editor, service.Dir()).Run(); err != nil {
			return fmt.Errorf("cannot open service: %w", err)
		}
		return nil
	},
}

func init() {
	ServiceCmd.Flags().StringVar(&editor, "editor", "code", "your editor: 'code' for vscode, 'goland' for goland")
}
