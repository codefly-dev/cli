package create

import (
	"fmt"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

type Global struct {
	org                  string
	domain               string
	createDefaultProject bool
	defaultProjectRoot   string
	defaultProjectName   string
}

func (g *Global) ProjectBuilder() configurations.ProjectBuilder {
	// TODO implement me
	panic("implement me")
}

func NewGlobal() *Global {
	return &Global{
		createDefaultProject: true,
		defaultProjectRoot:   configurations.HomeDir(),
		defaultProjectName:   "default",
	}
}

func (g *Global) DefaultProjectName() string {
	return g.defaultProjectName
}

func (g *Global) ProjectGetter() configurations.ProjectBuilder {
	return &configurations.ProjectInput{
		Name: g.DefaultProjectName(),
	}
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

	var relativeProjectRoot string
	prompt = &survey.Input{
		Message: "Where from ~ do you want to store your projects by default?",
		Default: "codefly",
	}
	err = survey.AskOne(prompt, &relativeProjectRoot)
	if err != nil {
		return logger.Wrapf(err, "cannot ask for project root")
	}
	g.defaultProjectRoot = relativeProjectRoot
	confirm := &survey.Confirm{
		Message: fmt.Sprintf("Do you want to create a default project at <%s/default>?", relativeProjectRoot),
		Default: true,
	}
	err = survey.AskOne(confirm, &g.createDefaultProject)
	if err != nil {
		return logger.Wrapf(err, "cannot ask for default project")
	}
	if !g.createDefaultProject {
		golor.Println(`You can create a new project later with
#(blue)[codefly create project <name>]`)
	}
	return nil
}

func (g *Global) Organization() string {
	return g.org
}

func (g *Global) Domain() string {
	return g.domain
}

func (g *Global) CreateDefaultProject() bool {
	return g.createDefaultProject
}
