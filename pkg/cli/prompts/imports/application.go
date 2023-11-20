package imports

import (
	"path/filepath"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/codefly-dev/cli/pkg/imports"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

func NewApplicationImporter(dir string) (*ApplicationImporter, error) {
	logger := shared.NewLogger("import.NewApplicationImporter")
	err := shared.CheckDirectory(dir)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot check import directory")
	}
	return &ApplicationImporter{dir: dir, name: filepath.Base(dir)}, nil
}

type ApplicationImporter struct {
	dir      string
	importer imports.SourceImporter

	project string
	name    string
	style   configurations.ProjectStyle
}

func (p *ApplicationImporter) Source() imports.SourceImporter {
	return p.importer
}

func (p *ApplicationImporter) ProjectName() string {
	return p.project
}

func (p *ApplicationImporter) NewApplicationName() string {
	return p.name
}

func (p *ApplicationImporter) Style() configurations.ProjectStyle {
	return p.style
}

func ExtractName(repository string) string {
	tokens := strings.Split(repository, "/")
	return tokens[len(tokens)-1]
}

func (p *ApplicationImporter) Fetch() error {
	logger := shared.NewLogger("import.ApplicationImporter.Fetch")

	if p.dir != "" {
		imp, err := imports.NewLocalSourceImporter(p.dir)
		if err != nil {
			return logger.Wrapf(err, "cannot create local source importer")
		}
		p.importer = imp
	} else {
		logger.TODO("wire the git stuff")
		//pr := survey.Input{Message: "Git repository:", Default: "https://github.com/WordPress/wordpress-develop"}
		//err := survey.AskOne(&pr, &p.repo)
		//if err != nil {
		//	return logger.Wrapf(err, "cannot ask repository")
		//}
		//p.name = ExtractName(p.repo)
	}

	projects, err := configurations.ListProjects()
	if err != nil {
		return logger.Wrapf(err, "list projects for options")
	}
	project, err := configurations.CurrentProject()
	if err != nil {
		return logger.Wrapf(err, "cannot get current project TODO")
	}
	current := project.Name
	options := []string{current}
	for _, project := range projects {
		if project.Name == current {
			continue
		}
		options = append(options, project.Name)
	}
	sel := survey.Select{Message: "Project:", Options: options}
	err = survey.AskOne(&sel, &p.project)
	if err != nil {
		return logger.Wrapf(err, "cannot ask project")
	}
	pr := survey.Input{Message: "New applications name:", Default: p.name}
	err = survey.AskOne(&pr, &p.name)
	if err != nil {
		return logger.Wrapf(err, "cannot ask applications name")
	}
	return nil
}
