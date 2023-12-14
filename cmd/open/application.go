package open

import (
	"os/exec"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ApplicationCmd represents the run command
var ApplicationCmd = &cobra.Command{
	Use:   "application",
	Short: "Open a application in your editor",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		application := common.Application(ctx)
		c := exec.Command(editor, application.Dir())
		err := c.Run()
		shared.UnexpectedExitOnError(err, "cannot open application")
	},
}

func init() {
	ApplicationCmd.Flags().StringVar(&editor, "editor", "code", "your editor: 'code' for vscode, 'goland' for goland")
}
