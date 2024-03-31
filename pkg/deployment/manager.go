package deployment

import (
	"context"

	"github.com/codefly-dev/core/configurations"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
)

func GetDeployment(ctx context.Context, project *configurations.Project, service *configurations.Service, environment *configurations.Environment, namespace string) (*builderv0.Deployment, error) {
	return &builderv0.Deployment{
		Namespace: namespace,
		Kind: &builderv0.Deployment_Kustomize{
			Kustomize: &builderv0.KustomizeDeployment{
				Destination: DirFor(ctx, project, builderv0.DeploymentKind_KUSTOMIZE),
			},
		},
	}, nil

}
