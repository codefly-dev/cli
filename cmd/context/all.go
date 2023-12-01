package context

import (
	"fmt"

	"github.com/codefly-dev/cli/pkg/cli/display"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// AllCmd represents the run command
var AllCmd = &cobra.Command{
	Use:   "all",
	Short: "Show current configuration",
	Run: func(cmd *cobra.Command, args []string) {
		ShowAll()
	},
}

func ShowAll() {
	logger := shared.NewLogger("context.all")
	project, err := configurations.CurrentProject()
	if err != nil {
		logger.Oops("No current project: TODO: Select or create default: %v", err)
		return
	}

	display.ShowProject(project, true)
	fmt.Println()

	projects, err := configurations.ListProjects()
	shared.ExitOnError(err, "Cannot list projects")
	var others []*configurations.Project
	for _, p := range projects {
		if p.Name == project.Name {
			continue
		}
		others = append(others, p)
	}
	for _, p := range others {
		display.ShowProject(p, false)
		fmt.Println()
	}
}
