package templates

import (
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/golor"
)

type ServiceDisplay struct {
	Service *configurations.ServiceReference
	Action  string
	Indent  string
}

func displayService(svc *configurations.ServiceReference, indent string) {
	golor.Println(`{{.Indent}}#(white,bold)[{{.Service.Name}}]`, &ServiceDisplay{Service: svc, Action: "Service", Indent: indent})
}

type DestinationExistsMessage struct {
	Destination string
}

func DestinationExists(msg DestinationExistsMessage) {
	golor.Println(`#(bold,cyan)[Service already found at <{{.Destination}}>. Use --override option. Exiting.]`)
}
