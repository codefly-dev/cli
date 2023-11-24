package application

import (
	"context"

	"github.com/codefly-dev/cli/pkg/actions/actions"
	v1actions "github.com/codefly-dev/cli/proto/v1/actions"
	"github.com/codefly-dev/core/configurations"
)

const AddApplication = "application.add"

type AddApplicationAction struct {
	*v1actions.AddApplication
}

type AddApplicationOutput struct {
	*v1actions.AddApplicationOutput
	*configurations.Application
}

func NewAddApplicationAction(in *v1actions.AddApplication) *AddApplicationAction {
	in.Kind = AddApplication
	return &AddApplicationAction{
		AddApplication: in,
	}
}

var _ actions.Action = (*AddApplicationAction)(nil)

func (action *AddApplicationAction) Run(ctx context.Context) (any, error) {
	p, err := configurations.NewApplication(action.Name)
	if err != nil {
		return nil, err
	}
	return AddApplicationOutput{
		AddApplicationOutput: &v1actions.AddApplicationOutput{Name: p.Name},
		Application:          p,
	}, nil
}

func init() {
	actions.RegisterFactory(AddApplication, actions.Wrap[*AddApplicationAction]())
}
