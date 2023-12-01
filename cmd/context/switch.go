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
		currentProject := common.ProjectConfiguration(true)
		if switchProject {
			projects, err := configurations.ListProjects()
			shared.ExitOnError(err, "cannot list projects")
			if len(projects) == 0 {
				return
			}
			var options []string
			var def string
			if currentProject != nil {
				logger.Debugf("currentProject project: %s", currentProject.Name)
				def = fmt.Sprintf("%s*", currentProject.Name)
				options = append(options, def)
			}
			for _, project := range projects {
				if project.Name == currentProject.Name {
					continue
				}
				options = append(options, project.Name)
			}
			prompt := &survey.Select{
				Message: "Choose a project:",
				Options: options,
				Default: def,
			}
			var selected string
			err = survey.AskOne(prompt, &selected)
			shared.ExitOnError(err, "cannot ask for projects")
			if selected == def {
				return
			}
			project, err := configurations.LoadProjectFromName(selected)
			shared.ExitOnError(err, "cannot load projects")
			configurations.Global().SetCurrentProject(project)
			return
		}

		if currentProject == nil {
			golor.Println(`#(cyan,bold)[No currentProject project. Use the --project option to pin one.]`)
			os.Exit(0)
		}

		apps, err := configurations.ListApplications()
		shared.ExitOnError(err, "cannot list applications")
		if len(apps) == 0 {
			return
		}

		currentApplication := common.ApplicationConfiguration(true)
		var def string
		var options []string
		if currentApplication != nil {
			def = fmt.Sprintf("%s*", currentApplication.Name)
			options = append(options, def)
		}
		for _, app := range apps {
			if app.Name == currentApplication.Name {
				continue
			}
			options = append(options, app.Name)
		}
		prompt := &survey.Select{
			Message: "Choose an applications:",
			Options: options,
			Default: def,
		}
		var selected string
		err = survey.AskOne(prompt, &selected)
		shared.ExitOnError(err, "cannot ask for applications")
		if selected == def {
			return
		}
		app, err := configurations.LoadApplicationFromName(selected)
		shared.ExitOnError(err, "cannot load applications")
		configurations.SetCurrentApplication(app)
	},
}

var switchProject bool

func init() {
	SwitchCmd.PersistentFlags().BoolVarP(&switchProject, "project", "p", false, "switch current project")
}
