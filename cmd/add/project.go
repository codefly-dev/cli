package add

import (
	"context"

	"github.com/codefly-dev/cli/pkg/actions/project"
	"github.com/codefly-dev/cli/pkg/cli/display"
	v1actions "github.com/codefly-dev/cli/proto/v1/actions"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ProjectCmd represents the run command
var ProjectCmd = &cobra.Command{
	Use:   "project",
	Short: "Add an project",

	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()
		if len(args) == 0 {
			shared.Exit("you must provide a project name")
		}
		name := args[0]

		action := project.NewAddProjectAction(&v1actions.AddProject{Name: name})
		result, err := action.Run(ctx)
		shared.ExitOnError(err, "cannot create project")

		output, ok := result.(project.AddProjectOutput)
		shared.ExitOnFalse(ok, "cannot cast result to project.AddProjectOutput: %T", output)

		display.CreatedProject(output.Project)
		configurations.MustCurrent().CurrentProject = output.Project.Name
		configurations.SaveCurrent()
	},
}

var style string

func init() {
	ProjectCmd.PersistentFlags().StringVar(&style, "style", "monorepo", "style of your project: monorepo, microservices, ...")
	ProjectCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
}
