package create

import (
	"fmt"

	"github.com/AlecAivazis/survey/v2"
	"github.com/codefly-dev/core/shared"
)

type Overrider struct{}

func NewOverrider() *Overrider {
	return &Overrider{}
}

func (o *Overrider) Override(p string) bool {
	override := false
	prompt := &survey.Confirm{
		Message: fmt.Sprintf("The file <%s> already exists. Do you want to override it?", p),
		Default: true,
	}
	err := survey.AskOne(prompt, &override)
	shared.UnexpectedExitOnError(err, "cannot ask for override")
	return override
}
