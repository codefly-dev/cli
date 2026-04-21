package deployments

import (
	"context"
	"os"
	"path"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"gopkg.in/yaml.v3"
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
			// Import images into k3d if applicable.
			if err := l.importImagesIfK3d(ctx, module, service); err != nil {
				w.Warn("k3d image import failed (continuing)", wool.Field("error", err.Error()))
			}

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

// importImagesIfK3d detects k3d and imports built Docker images into the cluster.
func (l *LocalApplyManager) importImagesIfK3d(ctx context.Context, module *resources.Module, service *resources.Service) error {
	cluster := DetectK3dCluster(ctx)
	if cluster == "" {
		return nil
	}

	// Read the kustomize overlay to find image references.
	dir := KustomizeDirForEnv(ctx, l.Workspace, module, service, l.Env)
	images, err := extractKustomizeImages(dir)
	if err != nil {
		return err
	}

	return K3dImportImages(ctx, cluster, images)
}

// kustomization is a minimal representation of kustomization.yaml for image extraction.
type kustomization struct {
	Images []kustomizeImage `yaml:"images"`
}

type kustomizeImage struct {
	Name    string `yaml:"name"`
	NewName string `yaml:"newName"`
	NewTag  string `yaml:"newTag"`
}

// extractKustomizeImages reads a kustomization.yaml and returns the image references.
func extractKustomizeImages(dir string) ([]string, error) {
	data, err := os.ReadFile(path.Join(dir, "kustomization.yaml"))
	if err != nil {
		return nil, err
	}

	var k kustomization
	if err := yaml.Unmarshal(data, &k); err != nil {
		return nil, err
	}

	var images []string
	for _, img := range k.Images {
		ref := img.NewName
		if img.NewTag != "" {
			ref += ":" + img.NewTag
		}
		if ref != "" {
			images = append(images, ref)
		}
	}
	return images, nil
}
