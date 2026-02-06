package deployments

import (
	"context"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/wool"
)

type Manager interface {
	Handle(ctx context.Context, service *resources.Service, module *resources.Module, deploy *builderv0.DeploymentOutput) error
}

func GetKubernetesDeployment(ctx context.Context, dockerBuildContext *builderv0.DockerBuildContext, workspace *resources.Workspace, module *resources.Module, service *resources.Service, env *resources.Environment, namespace string) (*builderv0.Deployment, error) {
	return &builderv0.Deployment{
		Kind: &builderv0.Deployment_Kubernetes{
			Kubernetes: &builderv0.KubernetesDeployment{
				BuildContext: dockerBuildContext,
				Namespace:    namespace,
				Destination:  KustomizeDir(ctx, workspace, module, service),
			},
		},
	}, nil
}

func NewLocalApplyManager(ctx context.Context, workspace *resources.Workspace, env *resources.Environment) *LocalApplyManager {
	return &LocalApplyManager{
		Workspace: workspace,
		Env:       env,
	}
}

type LocalApplyManager struct {
	Workspace *resources.Workspace
	Env       *resources.Environment
}

func (l *LocalApplyManager) Handle(ctx context.Context, service *resources.Service, module *resources.Module, deploy *builderv0.DeploymentOutput) error {
	w := wool.Get(ctx).In("Builder")
	switch v := deploy.Kind.(type) {
	case *builderv0.DeploymentOutput_Kubernetes:
		if v.Kubernetes.Kind == builderv0.KubernetesDeploymentOutput_Kustomize {
			err := l.KustomizeApply(ctx, module, service)
			if err != nil {
				return w.Wrapf(err, "cannot apply kustomize")
			}
		}
	default:
		return w.NewError("unsupported deployment kind %T", deploy.Kind)
	}
	return nil
}

var _ Manager = &LocalApplyManager{}

func (l *LocalApplyManager) KustomizeApply(ctx context.Context, module *resources.Module, service *resources.Service) error {
	w := wool.Get(ctx).In("Builder", wool.ThisField(resources.WithUnique(service)))
	dir := KustomizeDirForEnv(ctx, l.Workspace, module, service, l.Env)

	err := KustomizeApply(ctx, service, l.Env, dir)
	if err != nil {
		return w.Wrapf(err, "cannot apply kustomize")
	}
	return nil
}
