package templates

import (
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/golor"
)

type ModuleDisplay struct {
	Module  *resources.Module
	Action  string
	Current string
	Indent  string
}

func ModuleLoading(app *resources.Module) {
	module(ModuleDisplay{Module: app, Action: "Loading module"})
}

func ShowModule(app *resources.Module, current bool, indent string) {
	currentSign := ""
	if current {
		currentSign = "#(green)[*]"
	}
	module(ModuleDisplay{Module: app, Action: "Name", Current: currentSign, Indent: indent})
}

func module(d ModuleDisplay) {
	golor.Template(d).Println(`  #(blue)[{{.Indent}}{{.Action}} <{{.Module.Name}}{{.Current}}>]`)
	for _, svc := range d.Module.ServiceReferences {
		displayService(svc, d.Indent+"  - ")
	}
}

func ModuleWithNothingToRun() {
	golor.Println(`#(blue,bold)[Nothing to run!]: #(italic,white)[Add a Name]
#(italic)[codefly create service <name> --base=<base>]
Checkout https://codefly.build to find out what services you can create.
For go services, you can use the following base images: codefly.io/go
#(blue,italic)[codefly create service <name> --base=codefly.io/go]

Soon to come: interactive mode: don't leave the comfort of your terminal!
`)
}
