package create

import (
	"fmt"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

type Global struct {
	org    string
	domain string
}

func NewGlobal() *Global {
	return &Global{}
}

func (g *Global) Fetch() error {
	logger := shared.NewLogger("create.Global.Fetch")
	prompt := &survey.Input{
		Message: "Please enter your organization name:",
		Default: "codefly.ai",
	}
	err := survey.AskOne(prompt, &g.org)
	if err != nil {
		return logger.Wrapf(err, "cannot ask for organization name")
	}
	err = configurations.ValidateOrganization(g.org)
	if err != nil {
		return logger.Wrapf(err, "invalid organization name")
	}
	prompt = &survey.Input{
		Message: "Please enter your domain:",
		Default: fmt.Sprintf("github.com/%s", strings.Replace(g.org, ".", "-", -1)),
	}
	err = survey.AskOne(prompt, &g.domain)
	if err != nil {
		return logger.Wrapf(err, "cannot ask for domain")
	}

	return nil
}

func (g *Global) Organization() string {
	return g.org
}

func (g *Global) Domain() string {
	return g.domain
}

var _ configurations.GlobalConfigurationInputer = (*Global)(nil)
