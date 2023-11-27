package display

import (
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

type ProjectDisplay struct {
	Project *configurations.Project
	Action  string
	Current string
}

func ShowProject(project *configurations.Project, current bool) {
	currentSign := ""
	if current {
		currentSign = "#(green)[*]"
	}
	d := ProjectDisplay{Project: project, Action: "Project", Current: currentSign}
	golor.Println(`#(blue,bold)[🔎 Project <#bold[{{.Project.Name}}{{.Current}}]>]`, d)

	apps, err := configurations.ListApplications()
	shared.ExitOnError(err, "Cannot list applications")

	for _, app := range apps {
		current := project.Current() == app.Name
		ShowApplication(app, current, "  ")
	}
}

func CreatedProject(project *configurations.Project) {
	d := ProjectDisplay{Project: project}
	golor.Println(`#(blue,bold)[🔎 Created project #bold[{{.Project.Name}}]]`, d)
}
