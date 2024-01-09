package templates

import (
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/golor"
)

func PartialLoading(p *configurations.Partial) {
	golor.Println(`#(blue)[Loading partial #bold[%s] with applications]
{{- range .Applications }}
#(white,bold)[- %s]{{end}}`, p)
}
