package project

import (
	"context"

	v1actions "github.com/codefly-dev/cli/proto/v1/go/actions"

	"github.com/codefly-dev/cli/pkg/actions/actions"
	"github.com/codefly-dev/core/configurations"
)

const AddProject = "project.add"

type AddProjectAction struct {
	*v1actions.AddProject
}

type AddProjectOutput struct {
	*v1actions.AddProjectOutput
	*configurations.Project
}

func NewAddProjectAction(in *v1actions.AddProject) *AddProjectAction {
	in.Kind = AddProject
	return &AddProjectAction{
		AddProject: in,
	}
}

var _ actions.Action = (*AddProjectAction)(nil)

func (action *AddProjectAction) Run(ctx context.Context) (any, error) {
	p, err := configurations.NewProject(action.Name)
	if err != nil {
		return nil, err
	}
	return AddProjectOutput{
		AddProjectOutput: &v1actions.AddProjectOutput{Name: p.Name},
		Project:          p,
	}, nil
}

func init() {
	actions.RegisterFactory(AddProject, actions.Wrap[*AddProjectAction]())
}
