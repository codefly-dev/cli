package deployment

import (
	"context"

	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
	"github.com/codefly-dev/core/resources"
)

func GetKubernetesDeployment(ctx context.Context, dockerBuildContext *builderv0.DockerBuildContext, workspace *resources.Workspace, service *resources.Service, environment *resources.Environment, namespace string, withLoadBalancer bool) (*builderv0.Deployment, error) {
	return &builderv0.Deployment{
		LoadBalancer: withLoadBalancer,
		Kind: &builderv0.Deployment_Kubernetes{
			Kubernetes: &builderv0.KubernetesDeployment{
				BuildContext: dockerBuildContext,
				Namespace:    namespace,
				Destination:  Dir(ctx, workspace),
			},
		},
	}, nil

}
