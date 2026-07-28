package deployments

import (
	"context"
	"os"
	"path"
	"sync"

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

func NewLocalApplyManager(ctx context.Context, workspace *resources.Workspace, env *resources.Environment) (*LocalApplyManager, error) {
	target, err := VerifyLocalK3dTarget(ctx, env)
	if err != nil {
		return nil, err
	}
	return &LocalApplyManager{
		Workspace: workspace,
		Env:       env,
		target:    target,
		digests:   map[string]string{},
	}, nil
}

type LocalApplyManager struct {
	Workspace *resources.Workspace
	Env       *resources.Environment
	target    VerifiedKubernetesTarget
	mu        sync.Mutex
	digests   map[string]string
}

func (l *LocalApplyManager) Handle(ctx context.Context, service *resources.Service, module *resources.Module, deploy *builderv0.DeploymentOutput) error {
	w := wool.Get(ctx).In("Builder")
	switch v := deploy.Kind.(type) {
	case *builderv0.DeploymentOutput_Kubernetes:
		if v.Kubernetes.Kind == builderv0.KubernetesDeploymentOutput_KUSTOMIZE {
			if err := VerifyLocalK3dTargetUnchanged(ctx, l.Env, l.target); err != nil {
				return w.Wrapf(err, "cannot verify Kubernetes target before image import")
			}

			tree := KustomizeDir(ctx, l.Workspace, module, service)
			digest, err := RenderedTreeDigest(tree)
			if err != nil {
				return w.Wrapf(err, "cannot digest rendered deployment tree")
			}
			l.mu.Lock()
			l.digests[tree] = digest
			l.mu.Unlock()

			if err := l.importImages(ctx, module, service); err != nil {
				return w.Wrapf(err, "cannot import images into verified k3d cluster")
			}
			if err := l.KustomizeApply(ctx, module, service); err != nil {
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
	tree := KustomizeDir(ctx, l.Workspace, module, service)
	dir := KustomizeDirForEnv(ctx, l.Workspace, module, service, l.Env)
	l.mu.Lock()
	digest := l.digests[tree]
	l.mu.Unlock()

	err := KustomizeApply(ctx, service, l.Env, l.target, tree, digest, dir)
	if err != nil {
		return w.Wrapf(err, "cannot apply kustomize")
	}
	return nil
}

func (l *LocalApplyManager) importImages(ctx context.Context, module *resources.Module, service *resources.Service) error {
	dir := KustomizeDirForEnv(ctx, l.Workspace, module, service, l.Env)
	images, err := extractKustomizeImages(dir)
	if err != nil {
		return err
	}
	if err := EnsureImagesAvailable(ctx, images); err != nil {
		return err
	}
	if err := VerifyLocalK3dTargetUnchanged(ctx, l.Env, l.target); err != nil {
		return err
	}
	return K3dImportImages(ctx, l.target.K3dCluster, images)
}

func (l *LocalApplyManager) Target() VerifiedKubernetesTarget {
	return l.target
}

func (l *LocalApplyManager) RenderedDigest(ctx context.Context, module *resources.Module, service *resources.Service) (string, bool) {
	tree := KustomizeDir(ctx, l.Workspace, module, service)
	l.mu.Lock()
	defer l.mu.Unlock()
	digest, ok := l.digests[tree]
	return digest, ok
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
