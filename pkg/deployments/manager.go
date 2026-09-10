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
	// Stage is the furthest completion stage this tree verified. It never
	// reflects the stage the caller asked for: a stage beyond rendered is set
	// only after the apply or the observation that established it returned
	// successfully.
	Stage CompletionStage
	// Mutated records that this tree changed the target, even when it did not
	// reach StageApplied. A bootstrap barrier that fails has applied the
	// preparation resources and deliberately withheld the rollout, so the
	// namespace holds a partial revision an operator has to reconcile; without
	// this, StageRendered alone would read as "no cluster was contacted".
	Mutated     bool
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
	if completion.Timeout <= 0 {
		completion.Timeout = DefaultCompletionTimeout
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
	budget     observationBudget
	evidence   evidenceRecorder
}

// observationBudget is the completion timeout spent across every tree and every
// stage this manager observes. One manager serves a whole module deploy, so a
// per-call budget would let a caller's stated timeout be spent once per service
// per stage instead of once.
type observationBudget struct {
	mu    sync.Mutex
	spent time.Duration
}

func (l *LocalApplyManager) claimObservation() (time.Duration, error) {
	l.budget.mu.Lock()
	defer l.budget.mu.Unlock()
	remaining := l.completion.Timeout - l.budget.spent
	if remaining <= 0 {
		return 0, fmt.Errorf("observation budget of %s is exhausted", l.completion.Timeout)
	}
	return remaining, nil
}

func (l *LocalApplyManager) spendObservation(elapsed time.Duration) {
	l.budget.mu.Lock()
	defer l.budget.mu.Unlock()
	l.budget.spent += elapsed
}

// observe runs one observation against the deployment-wide budget and records
// what it read, whether or not the stage was established.
func (l *LocalApplyManager) observe(
	ctx context.Context,
	observer completionObserver,
	evidence *RenderedTreeEvidence,
	targets []ownedResource,
	stage CompletionStage,
) error {
	if len(targets) == 0 {
		return nil
	}
	budget, err := l.claimObservation()
	if err != nil {
		evidence.Diagnostics = append(evidence.Diagnostics, err.Error())
		return err
	}
	start := time.Now()
	observed, observeErr := observer.await(ctx, targets, stage, budget)
	l.spendObservation(time.Since(start))
	evidence.ObservedAt = time.Now().UTC()
	evidence.Resources = append(evidence.Resources, observed...)
	if observeErr != nil {
		evidence.Diagnostics = append(evidence.Diagnostics, observeErr.Error())
	}
	return observeErr
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

// partialRevision is what an operator has to act on when a bootstrap barrier
// fails: the preparation resources are live on the target and the workloads that
// go with them are not.
const partialRevision = "the preparation resources were applied and the workload rollout was withheld; " +
	"the target holds a partial revision"

// applyTree takes one rendered tree as far as the caller's completion condition
// requires, recording what it actually established at every exit.
//
// A caller that requires bootstrapping gets a barrier: the schema Jobs are
// applied and awaited before any workload rollout is applied, so a consumer
// never starts against preparation that has not finished. A caller that requires
// only StageApplied gets its documents applied exactly as rendered — the barrier
// reorders resources, and reordering is a behavior change nobody asked for.
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

	barrier := l.completion.Stage.AtLeast(StageBootstrapped)
	plan, err := planApply(documents, l.target.Namespace, barrier)
	if err != nil {
		return err
	}
	observer := completionObserver{env: l.Env, target: &l.target}

	// Mutated is set the moment an apply is attempted with something to apply,
	// not once it succeeds: KubernetesApply sends resources one at a time, so a
	// failure partway through has already changed the target. Over-reporting
	// prompts a check that finds nothing; under-reporting tells an operator to
	// skip a namespace that needs reconciling.
	if len(plan.Preparation) > 0 {
		evidence.Mutated = true
		if err := KubernetesApply(ctx, l.Env, &l.target, plan.Preparation...); err != nil {
			return err
		}
	}

	bootstrapped := false
	if barrier {
		if err := l.observe(ctx, observer, &evidence, plan.BootstrapJobs, StageBootstrapped); err != nil {
			evidence.Diagnostics = append(evidence.Diagnostics, partialRevision)
			return err
		}
		bootstrapped = true
	}
	if len(plan.Rollout) > 0 {
		evidence.Mutated = true
		if err := KubernetesApply(ctx, l.Env, &l.target, plan.Rollout...); err != nil {
			return err
		}
	}
	evidence.AppliedAt = time.Now().UTC()
	evidence.Stage = StageApplied
	if bootstrapped {
		evidence.Stage = StageBootstrapped
	}
	if !l.completion.Stage.AtLeast(StageHealthy) {
		return nil
	}
	// The bootstrap Jobs are deliberately absent here: their stage is already
	// established, and re-reading a completed Job that its TTL has since removed
	// would fail a deployment that succeeded.
	if err := l.observe(ctx, observer, &evidence, plan.RolloutTargets, StageHealthy); err != nil {
		return err
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
