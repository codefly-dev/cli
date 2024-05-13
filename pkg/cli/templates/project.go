package templates

import (
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/golor"
)

type WorkspaceDisplay struct {
	Workspace *resources.Workspace
	Action    string
	Current   string
}

func ShowWorkspace(workspace *resources.Workspace, current bool) {
	currentSign := ""
	if current {
		currentSign = "#(green)[*]"
	}
	d := WorkspaceDisplay{Workspace: workspace, Action: "Workspace", Current: currentSign}
	golor.Template(d).Println(`#(blue,bold)[🔎 Workspace <#bold[{{.Workspace.Name}}{{.Current}}]>]`)

	//mods, err := resources.ListModules()
	//shared.ExitOnError(err, "Cannot list modules")
	//
	//for _, mod :=  range apps {
	//	current := workspace.Current() == app.Name
	//	ShowModule(app, current, "  ")
	//}
}

func CreatedWorkspace(workspace *resources.Workspace) {
	d := WorkspaceDisplay{Workspace: workspace}
	golor.Template(d).Println(`#(blue,bold)[🔎 Created workspace #bold[{{.Workspace.Name}}]]`)
}
