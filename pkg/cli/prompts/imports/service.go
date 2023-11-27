package imports

import (
	"fmt"

	"github.com/codefly-dev/cli/pkg/services"

	"github.com/AlecAivazis/survey/v2"
	"github.com/codefly-dev/cli/pkg/imports"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
)

type ServicePrompt struct {
	steps []*services.CreationInput
	*imports.Recommendation
	namespace string
	name      string
}

var _ imports.ServiceImporter = (*ServicePrompt)(nil)

func (p *ServicePrompt) Fetch(rec *imports.Recommendation) error {
	logger := shared.NewLogger("import.ServicePrompt.Fetch")

	golor.Println(`We are creating a service for you.`)

	pr := survey.Input{Message: "Select a name for your service (you can always change this later)", Default: rec.Name}
	err := survey.AskOne(&pr, &p.name)
	if err != nil {
		return logger.Wrapf(err, "cannot ask for namespace")
	}
	golor.Println(`Choose between these bases selected for you!
{{- range .Names }}
- #(cyan)[{{ .Name }}]
 {{ .Description }}{{ end }}
`, rec.Main)
	var options []string
	for _, base := range rec.Main.Names {
		options = append(options, base.Name)
	}
	sel := survey.Select{
		Message: "Choose a service agent to use",
		Options: options,
	}
	var agentInput string
	err = survey.AskOne(&sel, &agentInput)
	if err != nil {
		return logger.Wrapf(err, "cannot ask for base")
	}

	pr = survey.Input{Message: "Select a namespace to create the service in (you can always change this later)", Default: "default"}
	err = survey.AskOne(&pr, &p.namespace)
	if err != nil {
		return logger.Wrapf(err, "cannot ask for namespace")
	}
	agent, err := configurations.ParseAgent(configurations.AgentService, agentInput)
	if err != nil {
		return logger.Wrapf(err, "cannot parse agent")
	}

	var dependsOn []string
	if len(rec.Dependencies) > 0 {
		golor.Println(`
🥸We detected that you may want to setup dependencies for your service. This will be added to the same namespace`)
		// Detection of dependencies
		for _, dep := range rec.Dependencies {
			c := survey.Confirm{Message: fmt.Sprintf("Do you want to add %s?", dep.Name()), Default: true}
			var pick bool
			err := survey.AskOne(&c, &pick)
			if err != nil {
				return logger.Wrapf(err, "cannot ask for dependency")
			}
			var name string
			pr := survey.Input{Message: "Select a name for the dependency", Default: dep.Identifier}
			err = survey.AskOne(&pr, &name)
			if err != nil {
				return logger.Wrapf(err, "cannot ask for dependency name")
			}
			if pick {
				logger.Debugf("Adding dependency <%s>", name)
				p.steps = append(p.steps, &services.CreationInput{
					Name:      name,
					Namespace: p.namespace,
					Agent:     dep,
				})
				dependsOn = append(dependsOn, name)
			}
		}
	}
	p.steps = append(p.steps, &services.CreationInput{
		Name:      p.name,
		Namespace: p.namespace,
		Agent:     agent,
		Files:     rec.Main.Includes,
		DependsOn: dependsOn,
	})
	return nil
}

func (p *ServicePrompt) CreationInputs() ([]*services.CreationInput, error) {
	return p.steps, nil
}
