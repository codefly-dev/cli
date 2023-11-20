package context

import (
	"github.com/AlecAivazis/survey/v2"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// SwitchCmd represents the run command
var SwitchCmd = &cobra.Command{
	Use:   "switch",
	Short: "Switch current configuration",
	Run: func(cmd *cobra.Command, args []string) {

		if switchProject {
			projects, err := configurations.ListProjects()
			shared.ExitOnError(err, "cannot list projects")
			if len(projects) == 0 {
				return
			}

			var options []string
			for _, project := range projects {
				options = append(options, project.Name)
			}
			current := configurations.MustCurrentProject().Name
			prompt := &survey.Select{
				Message: "Choose a project:",
				Options: options,
				Default: current,
			}
			var selected string
			err = survey.AskOne(prompt, &selected)
			shared.ExitOnError(err, "cannot ask for projects")
			project, err := configurations.LoadProjectFromName(selected)
			shared.ExitOnError(err, "cannot load projects")
			configurations.SetCurrentProject(project)
			return
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
		current := configurations.MustCurrentApplication().Name
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
