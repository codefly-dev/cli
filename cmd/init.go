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
		if err != nil {
			//shared.ExitOnError(err, "cannot create default project")
		}
		configurations.AddProject(project)
		configurations.SetCurrentProject(project)
		golor.Println(`#(blue)[Project <default> created at ~/codefly/default!]`)

		// Creating a default application
		application, err := configurations.NewApplication("application")
		if err != nil {
			shared.ExitOnError(err, "cannot create default application")
		}
		configurations.AddApplication(application)
		configurations.SetCurrentApplication(application)
		golor.Println(`#(blue)[Application <application> created at ~/codefly/default/application!]
Add new services to your application with
#(cyan)[codefly add service {NAME} --plugin={PLUGIN}]
To get started, try adding a service that wraps an shell command with
#(cyan)[codefly add service {NAME} --plugin=codefly.ai/shell:latest]

Note: To quickly edit your services, run
#(cyan)[codefly open --editor={EDITOR}]
to open the project in your favorite IDE, vscode or others.`)

	},
}
