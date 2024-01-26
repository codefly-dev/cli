package templates

import (
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/golor"
)

type ApplicationDisplay struct {
	Application *configurations.Application
	Action      string
	Current     string
	Indent      string
}

func ApplicationLoading(app *configurations.Application) {
	application(ApplicationDisplay{Application: app, Action: "Loading application"})
}

func ShowApplication(app *configurations.Application, current bool, indent string) {
	currentSign := ""
	if current {
		currentSign = "#(green)[*]"
	}
	application(ApplicationDisplay{Application: app, Action: "Name", Current: currentSign, Indent: indent})
}

func application(d ApplicationDisplay) {
	golor.Template(d).Println(`  #(blue)[{{.Indent}}{{.Action}} <{{.Application.Name}}{{.Current}}>]`)
	for _, svc := range d.Application.Services {
		displayService(svc, d.Indent+"  - ")
	}
}

func ApplicationWithNothingToRun() {
	golor.Println(`#(blue,bold)[Nothing to run!]: #(italic,white)[Add a Name]
#(italic)[codefly create service <name> --base=<base>]
Checkout https://codefly.build to find out what services you can create.
For go services, you can use the following base images: codefly.io/go
#(blue,italic)[codefly create service <name> --base=codefly.io/go]

Soon to come: interactive mode: don't leave the comfort of your terminal!
`)
}
