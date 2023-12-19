package open

import (
	"os/exec"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Open a service in your editor",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		service := common.Service(ctx)
		c := exec.Command(editor, service.Dir())
		err := c.Run()
		cli.ExitOnError(err, "cannot open service")
	},
}

func init() {
	ServiceCmd.Flags().StringVar(&editor, "editor", "code", "your editor: 'code' for vscode, 'goland' for goland")
}
