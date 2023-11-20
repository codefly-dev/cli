package cmd

import (
	"github.com/codefly-dev/cli/pkg/cli/prompts/create"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// InitCmd represents the init command
var InitCmd = &cobra.Command{
	Use:   "init",
	Short: "eInit",
	Run: func(cmd *cobra.Command, args []string) {
		golor.Println(`#(blue)[Welcome to Codefly 🪽!]

#(italic,white)[🔜 Coming soon:] logging to get your configuration from the server.
`)
		getter := create.NewGlobal()
		//override := create.NewOverrider()
		configurations.InitGlobal(getter)

		golor.Println(`#(blue)[📝Important]
Passing around names of project, applications or service is very poor UX.
Codefly tries to avoid it by keeping the context of your current work, what project and applications you are working on.

To see your current context use the #(italic,white)[codefly context view] command.

You can always change the context by using the #(italic,white)[codefly context set] command.
`)
	},
}
