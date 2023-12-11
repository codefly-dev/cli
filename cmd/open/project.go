package open

import (
	"os/exec"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ProjectCmd represents the run command
var ProjectCmd = &cobra.Command{
	Use:   "project",
	Short: "Open a project in your editor",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		project := common.Project(ctx)
		c := exec.Command(editor, project.Dir())
		err := c.Run()
		shared.UnexpectedExitOnError(err, "cannot open project")

	},
}

func init() {
	ProjectCmd.Flags().StringVar(&editor, "editor", "code", "your editor: 'code' for vscode, 'goland' for goland")
}
