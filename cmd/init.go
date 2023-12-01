package cmd

import (
	"github.com/codefly-dev/cli/pkg/cli/prompts/create"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// InitCmd represents the init command
var InitCmd = &cobra.Command{
	Use:   "init",
	Short: "eInit",
	Run: func(cmd *cobra.Command, args []string) {
		golor.Println(`#(blue)[Welcome to Codefly 🪽!]`)
		getter := create.NewGlobal()
		configurations.InitGlobal(getter)
		// Creating a default project
		project, err := configurations.NewProject("default")
		shared.ExitOnError(err, "cannot create default project")
		configurations.Global().AddProject(project)
		configurations.Global().SetCurrentProject(project)
		golor.Println(`#(blue)[Project <default> created at ~/codefly/default!]`)

		// Creating a default app
		app, err := configurations.NewApplication("future")
		shared.ExitOnError(err, "cannot create default app")
		err = project.AddApplication(app.Reference())
		shared.ExitOnError(err, "cannot add default app")

		configurations.SetCurrentApplication(app)
		golor.Println(`#(blue)[Application <app> created at ~/codefly/default/app!]
Add new services to your app with
#(cyan)[codefly add service {NAME} --agent={AGENT}]
To get started, try adding a service that wraps an shell command with
#(cyan)[codefly add service {NAME} --agent=codefly.ai/shell:latest]

Note: To quickly edit your services, run
#(cyan)[codefly open --editor={EDITOR}]
to open the project in your favorite IDE, vscode or others.`)

	},
}
