package deployment

import (
	"context"

	"github.com/codefly-dev/core/configurations"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
)

func GetKubernetesDeployment(ctx context.Context, dockerBuildContext *builderv0.DockerBuildContext, project *configurations.Project, service *configurations.Service, environment *configurations.Environment, namespace string) (*builderv0.Deployment, error) {
	return &builderv0.Deployment{
		Kind: &builderv0.Deployment_Kubernetes{
			Kubernetes: &builderv0.KubernetesDeployment{
				BuildContext: dockerBuildContext,
				Namespace:    namespace,
				Destination:  Dir(ctx, project),
			},
		},
	}, nil

}
