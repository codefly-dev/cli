package deployments

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"gopkg.in/yaml.v3"
)

type Manager interface {
	Handle(ctx context.Context, service *resources.Service, module *resources.Module, deploy *builderv0.DeploymentOutput) error
}

type DeploymentOutputRequirement interface {
	RequiresDeploymentOutput() bool
}

func RequiresDeploymentOutput(manager Manager) bool {
	requirement, ok := manager.(DeploymentOutputRequirement)
	return ok && requirement.RequiresDeploymentOutput()
}

// RenderedTreeEvidence is what one deployment tree established: which target it
// was applied to, the digest of the artifact that produced it, when each stage
// was reached, the owned resources observation read, and — when a stage was not
// reached — the terminal diagnostics that explain why.
type RenderedTreeEvidence struct {
	Module    string
	Service   string
	Digest    string
	Manifests string
	// Stage is the furthest completion stage this tree established, never the
	// stage its caller asked for.
	Stage       CompletionStage
	RenderedAt  time.Time
	AppliedAt   time.Time
	ObservedAt  time.Time
	Resources   []ObservedResource
	Diagnostics []string
}

type DeploymentEvidence struct {
	Target        *VerifiedKubernetesTarget
	RenderedTrees []RenderedTreeEvidence
	// Required is the stage the caller asked for; Reached is the weakest stage
	// any rendered tree established.
	Required CompletionStage
	Reached  CompletionStage
}

type EvidenceProvider interface {
	Evidence() DeploymentEvidence
}

type evidenceKey struct {
	module  string
	service string
}

type evidenceRecorder struct {
	mu    sync.Mutex
	trees map[evidenceKey]RenderedTreeEvidence
}

func newEvidenceRecorder() evidenceRecorder {
	return evidenceRecorder{trees: map[evidenceKey]RenderedTreeEvidence{}}
}

func (r *evidenceRecorder) record(tree *RenderedTreeEvidence) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trees[evidenceKey{module: tree.Module, service: tree.Service}] = *tree
}

func (r *evidenceRecorder) renderedTrees() []RenderedTreeEvidence {
	r.mu.Lock()
	defer r.mu.Unlock()
	trees := make([]RenderedTreeEvidence, 0, len(r.trees))
	for key := range r.trees {
		trees = append(trees, r.trees[key])
	}
	sort.Slice(trees, func(i, j int) bool {
		if trees[i].Module == trees[j].Module {
			return trees[i].Service < trees[j].Service
		}
		return trees[i].Module < trees[j].Module
	})
	return trees
}

func GetKubernetesDeployment(
	ctx context.Context,
	dockerBuildContext *builderv0.DockerBuildContext,
	workspace *resources.Workspace,
	module *resources.Module,
	service *resources.Service,
	namespace string,
	profile builderv0.KubernetesOutputProfile,
	secretReferences map[string]*builderv0.KubernetesSecretKeyReference,
) (*builderv0.Deployment, error) {
	return &builderv0.Deployment{
		Kind: &builderv0.Deployment_Kubernetes{
			Kubernetes: &builderv0.KubernetesDeployment{
				BuildContext:       dockerBuildContext,
				Namespace:          namespace,
				Destination:        KustomizeDir(ctx, workspace, module, service),
				Profile:            profile,
				SecretReferences:   secretReferences,
				ValidateServerSide: profile == builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
			},
		},
	}, nil
}

func KubernetesOutputProfile(manager Manager) builderv0.KubernetesOutputProfile {
	if _, directLocalApply := manager.(*LocalApplyManager); directLocalApply {
		return builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1
	}
	return builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1
}

// NewLocalApplyManager binds a direct apply to an exact local k3d target and to
// the completion the caller requires. A direct apply always mutates a cluster,
// so a condition weaker than applied is a caller mistake rather than a
// render-only request.
func NewLocalApplyManager(
	ctx context.Context,
	workspace *resources.Workspace,
	env *resources.Environment,
	completion CompletionCondition,
) (*LocalApplyManager, error) {
	if !completion.Stage.AtLeast(StageApplied) {
		return nil, fmt.Errorf(
			"direct apply cannot complete at %q; use a render manager for %s, or require one of %s",
			completion.Stage,
			StageRendered,
			strings.Join(CompletionStageNames()[1:], ", "),
		)
	}
	target, err := VerifyLocalK3dTarget(ctx, env)
	if err != nil {
		return nil, err
	}
	return &LocalApplyManager{
		Workspace:  workspace,
		Env:        env,
		target:     target,
		completion: completion,
		evidence:   newEvidenceRecorder(),
	}, nil
}

type LocalApplyManager struct {
	Workspace  *resources.Workspace
	Env        *resources.Environment
	target     VerifiedKubernetesTarget
	completion CompletionCondition
	evidence   evidenceRecorder
}

func (l *LocalApplyManager) Handle(ctx context.Context, service *resources.Service, module *resources.Module, deploy *builderv0.DeploymentOutput) error {
	w := wool.Get(ctx).In("Builder")
	switch v := deploy.Kind.(type) {
	case *builderv0.DeploymentOutput_Kubernetes:
		if v.Kubernetes.Kind == builderv0.KubernetesDeploymentOutput_KUSTOMIZE {
			if err := VerifyLocalK3dTargetUnchanged(ctx, l.Env, &l.target); err != nil {
				return w.Wrapf(err, "cannot verify Kubernetes target before image import")
			}

			tree := KustomizeDir(ctx, l.Workspace, module, service)
			digest, err := RenderedTreeDigest(tree)
			if err != nil {
				return w.Wrapf(err, "cannot digest rendered deployment tree")
			}

			if err := l.importImages(ctx, module, service); err != nil {
				return w.Wrapf(err, "cannot import images into verified k3d cluster")
			}
			if err := l.applyTree(ctx, module.Name, service.Name, tree, digest, KustomizeDirForEnv(ctx, l.Workspace, module, service, l.Env)); err != nil {
				return w.Wrapf(err, "cannot apply kustomize")
			}
		}
	default:
		return w.NewError("unsupported deployment kind %T", deploy.Kind)
	}
	return nil
}

var _ Manager = &LocalApplyManager{}
var _ EvidenceProvider = &LocalApplyManager{}

// applyTree takes one rendered tree as far as the caller's completion condition
// requires, recording what it actually established at every exit. Preparation
// resources — Jobs included — are applied and, when the caller requires
// bootstrapping, awaited before any workload rollout is applied, so a consumer
// never starts against schema preparation that has not finished.
func (l *LocalApplyManager) applyTree(ctx context.Context, module, service, tree, digest, dir string) error {
	manifests, documents, err := renderKustomize(ctx, tree, digest, dir)
	if err != nil {
		return err
	}
	evidence := RenderedTreeEvidence{
		Module:     module,
		Service:    service,
		Digest:     digest,
		Manifests:  manifests,
		Stage:      StageRendered,
		RenderedAt: time.Now().UTC(),
	}
	defer func() { l.evidence.record(&evidence) }()

	owned, err := ownedResources(manifests, l.Env)
	if err != nil {
		return err
	}
	preparation, rollout, err := partitionDocuments(documents)
	if err != nil {
		return err
	}
	observer := completionObserver{env: l.Env, target: &l.target}

	if err := KubernetesApply(ctx, l.Env, &l.target, preparation...); err != nil {
		return err
	}
	if l.completion.Stage.AtLeast(StageBootstrapped) {
		observed, observeErr := observer.await(ctx, owned, StageBootstrapped, l.completion.Timeout)
		evidence.ObservedAt = time.Now().UTC()
		evidence.Resources = observed
		if observeErr != nil {
			evidence.Diagnostics = append(evidence.Diagnostics, observeErr.Error())
			return observeErr
		}
	}
	if err := KubernetesApply(ctx, l.Env, &l.target, rollout...); err != nil {
		return err
	}
	evidence.AppliedAt = time.Now().UTC()
	evidence.Stage = StageApplied
	if l.completion.Stage.AtLeast(StageBootstrapped) {
		evidence.Stage = StageBootstrapped
	}
	if !l.completion.Stage.AtLeast(StageHealthy) {
		return nil
	}
	observed, observeErr := observer.await(ctx, owned, StageHealthy, l.completion.Timeout)
	evidence.ObservedAt = time.Now().UTC()
	evidence.Resources = observed
	if observeErr != nil {
		evidence.Diagnostics = append(evidence.Diagnostics, observeErr.Error())
		return observeErr
	}
	evidence.Stage = StageHealthy
	return nil
}

func (l *LocalApplyManager) ApplyModuleKustomize(ctx context.Context, module *resources.Module, dir string) error {
	if err := VerifyLocalK3dTargetUnchanged(ctx, l.Env, &l.target); err != nil {
		return err
	}
	tree := path.Join(module.Dir(), "deployment", "kustomize")
	digest, err := RenderedTreeDigest(tree)
	if err != nil {
		return err
	}
	return l.applyTree(ctx, module.Name, "", tree, digest, dir)
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
	if err := VerifyLocalK3dTargetUnchanged(ctx, l.Env, &l.target); err != nil {
		return err
	}
	return K3dImportImages(ctx, l.target.K3dCluster, images)
}

func (l *LocalApplyManager) Evidence() DeploymentEvidence {
	target := l.target
	trees := l.evidence.renderedTrees()
	return DeploymentEvidence{
		Target:        &target,
		RenderedTrees: trees,
		Required:      l.completion.Stage,
		Reached:       weakestStage(trees),
	}
}

type RenderManager struct {
	Workspace *resources.Workspace
	Env       *resources.Environment
	evidence  evidenceRecorder
}

func NewRenderManager(workspace *resources.Workspace, env *resources.Environment) *RenderManager {
	return &RenderManager{
		Workspace: workspace,
		Env:       env,
		evidence:  newEvidenceRecorder(),
	}
}

func (r *RenderManager) Handle(ctx context.Context, service *resources.Service, module *resources.Module, deploy *builderv0.DeploymentOutput) error {
	w := wool.Get(ctx).In("Builder")
	switch v := deploy.Kind.(type) {
	case *builderv0.DeploymentOutput_Kubernetes:
		if v.Kubernetes.Kind != builderv0.KubernetesDeploymentOutput_KUSTOMIZE {
			return nil
		}
		tree := KustomizeDir(ctx, r.Workspace, module, service)
		digest, err := RenderedTreeDigest(tree)
		if err != nil {
			return w.Wrapf(err, "cannot digest rendered deployment tree")
		}
		manifests, _, err := renderKustomize(ctx, tree, digest, KustomizeDirForEnv(ctx, r.Workspace, module, service, r.Env))
		if err != nil {
			return w.Wrapf(err, "cannot render kustomize")
		}
		r.evidence.record(&RenderedTreeEvidence{
			Module:     module.Name,
			Service:    service.Name,
			Digest:     digest,
			Manifests:  manifests,
			Stage:      StageRendered,
			RenderedAt: time.Now().UTC(),
		})
	default:
		return w.NewError("unsupported deployment kind %T", deploy.Kind)
	}
	return nil
}

func (r *RenderManager) Evidence() DeploymentEvidence {
	trees := r.evidence.renderedTrees()
	return DeploymentEvidence{
		RenderedTrees: trees,
		Required:      StageRendered,
		Reached:       weakestStage(trees),
	}
}

var _ Manager = &RenderManager{}
var _ EvidenceProvider = &RenderManager{}

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
		if ref == "" {
			ref = img.Name
		}
		if img.NewTag != "" {
			if digest := strings.IndexByte(ref, '@'); digest >= 0 {
				ref = ref[:digest]
			}
			if tag := strings.LastIndexByte(ref, ':'); tag > strings.LastIndexByte(ref, '/') {
				ref = ref[:tag]
			}
			ref += ":" + img.NewTag
		}
		if ref != "" {
			images = append(images, ref)
		}
	}
	return images, nil
}
