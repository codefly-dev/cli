package deployment

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/configurations"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
)

// Manager maps project + environment to deployment details

func GetDeployment(ctx context.Context, project *configurations.Project, service *configurations.Service, environment *configurations.Environment) (*builderv0.Deployment, error) {
	namespace := fmt.Sprintf("%s-%s", project.Name, service.Application)
	return &builderv0.Deployment{
		Namespace: namespace,
		Kind: &builderv0.Deployment_Kustomize{
			Kustomize: &builderv0.KustomizeDeployment{
				Destination: DirFor(ctx, project, builderv0.DeploymentKind_KUSTOMIZE),
			},
		},
	}, nil

}
