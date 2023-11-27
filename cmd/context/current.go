package context

import (
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// CurrentCmd represents the run command
var CurrentCmd = &cobra.Command{
	Use:   "current",
	Short: "Show current configuration",
	Run: func(cmd *cobra.Command, args []string) {
		ShowCurrent()
	},
}

func ShowCurrent() {
	logger := shared.NewLogger("context.current")
	project, err := configurations.CurrentProject()
	if err != nil {
		logger.Oops("No current project: TODO: Select or create default: %v", err)
		return
	}
	golor.Println(`#(blue,bold)[🔎 Current View]: #(italic,white)[{{ .View }}]`,
		map[string]string{"Organization": project.Organization, "View": project.Name})

	app, err := project.CurrentApplication()
	if err != nil {
		golor.Println(`Nothing yet. Let's create one! 🚀
#(blue)[codefly create applications <name>]
`)
		return

	}
	golor.Println(`#(blue,bold[🌟 Current Name]: #(italic,white)[{{ .Name }}]`, map[string]string{
		"Organization": project.Organization,
		"View":         project.Name,
		"Name":         app.Name,
	})

	if len(app.Services) == 0 {
		golor.Println(`😵‍💫No services for this applications yet!
#(italic,white)[You can add a service to your applications by running]
codefly create service <service-name> --agent=<base>`)
		return
	}

	golor.Println(`#(blue,bold[🔥 Services for <{{.Name}}>]
{{- range .Services}}
- #(cyan,bold)[{{.Name}}]{{end}}`, map[string]any{
		"Name":     app.Name,
		"Services": app.Services,
	})

	// Other applications
	all, err := project.ListApplications()
	var others []*configurations.Application
	shared.ExitOnError(err, "cannot list applications")
	for _, other := range all {
		if other.Name == app.Name {
			continue
		}
		others = append(others, other)
	}
	if len(others) == 0 {
		return
	}
	golor.Println(`#(blue,bold[💥 Other applications]
{{- range .Others}}
- #(cyan,bold)[{{.Name}}]{{end}}`, map[string]any{
		"Others": others,
	})

	partials := project.Partials
	if len(partials) == 0 {
		return
	}
	golor.Println(`#(blue,bold[☄️ Partials]
{{- range .Partials}}
- #(cyan,bold)[{{.Name}}]{{end}}`, map[string]any{
		"Partials": partials,
	})
}
