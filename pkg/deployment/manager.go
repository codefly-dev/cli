package deployment

import (
	"context"

	"github.com/codefly-dev/core/configurations"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
)

// Manager maps project + environment to deployment details

type Manager interface {
	Deployment(ctx context.Context, project *configurations.Project, environment *configurations.Environment) (*builderv0.Deployment, error)
}

type LocalManager struct {
}

func (l LocalManager) Deployment(ctx context.Context, project *configurations.Project, environment *configurations.Environment) (*builderv0.Deployment, error) {
	return &builderv0.Deployment{
		Kind: &builderv0.Deployment_Kustomize{
			Kustomize: &builderv0.KustomizeDeployment{
				Destination: DirFor(ctx, project, builderv0.DeploymentKind_KUSTOMIZE),
			},
		},
	}, nil

}

var _ Manager = &LocalManager{}

func NewLocalManager(ctx context.Context) (*LocalManager, error) {
	return &LocalManager{}, nil
}
