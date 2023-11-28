package context

import (
	"fmt"
	"os"

	"github.com/AlecAivazis/survey/v2"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// SwitchCmd represents the run command
var SwitchCmd = &cobra.Command{
	Use:   "switch",
	Short: "Switch current configuration",
	Run: func(cmd *cobra.Command, args []string) {
		logger := shared.NewLogger("context.SwitchCmd")
		current := common.ProjectConfiguration(true)
		if switchProject {
			projects, err := configurations.ListProjects()
			shared.ExitOnError(err, "cannot list projects")
			if len(projects) == 0 {
				return
			}
			var options []string
			var currentProject string
			if current != nil {
				logger.Debugf("current project: %s", current.Name)
				currentProject = fmt.Sprintf("%s*", current.Name)
				options = append(options, currentProject)
			}
			for _, project := range projects {
				if project.Name == current.Name {
					continue
				}
				options = append(options, project.Name)
			}
			prompt := &survey.Select{
				Message: "Choose a project:",
				Options: options,
			}
			if currentProject != "" {
				prompt.Default = currentProject
			}
			var selected string
			err = survey.AskOne(prompt, &selected)
			shared.ExitOnError(err, "cannot ask for projects")
			if selected == currentProject {
				return
			}
			project, err := configurations.LoadProjectFromName(selected)
			shared.ExitOnError(err, "cannot load projects")
			configurations.SetCurrentProject(project)
			return
		}

		if current == nil {
			golor.Println(`#(cyan,bold)[No current project. Use the --project option to pin one.]`)
			os.Exit(0)
		}
		apps, err := configurations.ListApplications()
		shared.ExitOnError(err, "cannot list applications")
		if len(apps) == 0 {
			return
		}

		var options []string
		for _, app := range apps {
			options = append(options, app.Name)
		}
		prompt := &survey.Select{
			Message: "Choose an applications:",
			Options: options,
			Default: current,
		}
		var selected string
		err = survey.AskOne(prompt, &selected)
		shared.ExitOnError(err, "cannot ask for applications")
		app, err := configurations.LoadApplicationFromName(selected)
		shared.ExitOnError(err, "cannot load applications")
		configurations.SetCurrentApplication(app)
	},
}

var switchProject bool

func init() {
	SwitchCmd.PersistentFlags().BoolVarP(&switchProject, "project", "p", false, "switch current project")
}
