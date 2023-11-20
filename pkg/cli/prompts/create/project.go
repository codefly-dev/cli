package create

import (
	"github.com/AlecAivazis/survey/v2"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

func NewProjectBuilder(style string) (*ProjectBuilder, error) {
	return &ProjectBuilder{style: configurations.NewStyle(style)}, nil
}

type ProjectBuilder struct {
	name               string
	relativePath       string
	style              configurations.ProjectStyle
	defaultProjectName string
}

func (p *ProjectBuilder) ProjectName() string {
	return p.name
}

func (p *ProjectBuilder) RelativePath() string {
	return p.relativePath
}

func (p *ProjectBuilder) Style() configurations.ProjectStyle {
	return p.style
}

func (p *ProjectBuilder) Fetch() error {
	pr := survey.Input{Message: "Name:", Default: p.defaultProjectName}
	err := survey.AskOne(&pr, &p.name)
	shared.UnexpectedExitOnError(err, "cannot ask project name")
	err = configurations.ValidateProjectName(p.name)
	shared.ExitOnError(err, "invalid project name")
	dir := configurations.ProjectPath(p.name)
	pr = survey.Input{Message: "Location:", Default: dir}
	var location string
	err = survey.AskOne(&pr, &location)
	shared.UnexpectedExitOnError(err, "cannot ask project location")
	p.relativePath = configurations.RelativeProjectPath(location)
	return nil
}

func (p *ProjectBuilder) WithDefaultProjectName(s string) {
	p.defaultProjectName = s
}
