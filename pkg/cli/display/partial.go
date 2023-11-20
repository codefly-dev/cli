package display

import (
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/golor"
)

func PartialLoading(p *configurations.Partial) {
	golor.Println(`#(blue)[Loading partial #bold[{{.Name}}] with applications]
{{- range .Applications }}
#(white,bold)[- {{.}}]{{end}}`, p)
}
