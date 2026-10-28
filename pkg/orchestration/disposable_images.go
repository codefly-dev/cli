package orchestration

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/codefly-dev/core/architecture"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/proto"
)

// WithDisposableImages isolates host-executed recipe outputs for this flow.
// It does not authorize image deletion or imply that image inputs are isolated.
func (flow *Flow) WithDisposableImages() {
	if flow.world.disposableImageScope == "" {
		flow.world.disposableImageScope = NewInvocationID()
	}
}

func (world *World) checkDisposableImagePolicy() error {
	if world.disposableImageScope != "" && (world.Push || len(world.BuildCache.GetExports()) > 0) {
		return fmt.Errorf("disposable images do not support push or build-cache exports")
	}
	return nil
}

// CheckDisposableImageInputs admits the selected graph without loading agents.
// CI uses it before scheduling; InitManagers checks its final selected graph too.
func (flow *Flow) CheckDisposableImageInputs(ctx context.Context) error {
	if flow.world.disposableImageScope == "" {
		return nil
	}
	if err := flow.world.checkDisposableImagePolicy(); err != nil {
		return err
	}
	_, options, err := flow.dependencyGraphOptions(ctx)
	if err != nil {
		return err
	}
	dependencies := flow.world.Dependencies
	if len(options) > 0 {
		dependencies, err = architecture.NewServiceDependencies(ctx, flow.dependencyWorkspace(), options...)
		if err != nil {
			return err
		}
	}
	return checkDisposableImageDependencies(ctx, dependencies, flow.rootUniques())
}

func checkDisposableImageDependencies(ctx context.Context, dependencies *architecture.ServiceDependencies, roots []string) error {
	run, err := dependencies.ForStage(resources.StageRun)
	if err != nil {
		return err
	}
	var participants []string
	for _, root := range roots {
		required, err := run.OrderTo(ctx, root)
		if err != nil {
			return err
		}
		participants = append(participants, root)
		for _, service := range required {
			participants = append(participants, service.Unique)
		}
	}
	return CheckDisposableImageBuildPrerequisites(ctx, dependencies, participants)
}

// CheckDisposableImageBuildPrerequisites checks only the supplied participants'
// build closure. CI uses it for the roots whose build prerequisites it schedules;
// Flow supplies its selected runtime participants. It does not select runtime
// participants from an unfiltered workspace graph.
func CheckDisposableImageBuildPrerequisites(ctx context.Context, dependencies *architecture.ServiceDependencies, participants []string) error {
	build, err := dependencies.ForStage(resources.StageBuild)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, participant := range participants {
		if seen[participant] {
			continue
		}
		seen[participant] = true
		required, err := build.OrderTo(ctx, participant)
		if err != nil {
			return err
		}
		if len(required) > 0 {
			return fmt.Errorf("disposable image input rebinding is unsupported: %s has build prerequisite %s", participant, required[0].Unique)
		}
	}
	return nil
}

func (b *Builder) checkDisposableImagePlan(plan *builderv0.DockerBuildPlan) error {
	if err := b.world.checkDisposableImagePolicy(); err != nil {
		return err
	}
	if b.world.disposableImageScope != "" && len(plan.GetRecipes()) > 1 {
		return fmt.Errorf("disposable image input rebinding is unsupported: %s has %d recipes", b.instance.Unique(), len(plan.GetRecipes()))
	}
	return nil
}

// scopedRecipe retains one execution projection before identity is computed.
// The digest below only makes a name-safe service/recipe discriminator; it is
// not an input identity or an ownership receipt.
func (b *Builder) scopedRecipe(recipe *builderv0.DockerBuildRecipe) *builderv0.DockerBuildRecipe {
	if b.executionRecipes == nil {
		b.executionRecipes = map[string]*builderv0.DockerBuildRecipe{}
	}
	if existing := b.executionRecipes[recipe.GetName()]; existing != nil {
		return existing
	}
	execution := proto.CloneOf(recipe)
	discriminator := sha256.Sum256([]byte(b.instance.Unique() + "\x00" + recipe.GetName()))
	execution.Image = fmt.Sprintf("codefly-local/disposable/%s:%x", b.world.disposableImageScope, discriminator)
	b.executionRecipes[recipe.GetName()] = execution
	return execution
}

func (b *Builder) imageEvidencePlan(plan *builderv0.DockerBuildPlan) *builderv0.DockerBuildPlan {
	if plan == nil || b.world.disposableImageScope == "" {
		return plan
	}
	execution := proto.CloneOf(plan)
	for i, recipe := range execution.Recipes {
		if built := b.executionRecipes[recipe.GetName()]; built != nil {
			execution.Recipes[i] = proto.CloneOf(built)
		}
	}
	return execution
}

func (b *Builder) resetExecutionProjection() {
	b.executionRecipes = nil
	if b.world.disposableImageScope != "" {
		b.resolvedImages = nil
	}
}

func (b *Builder) checkDisposableImageIdentity(identity imageIdentity, cause error) error {
	if b.world.disposableImageScope != "" && (cause != nil || identity.Key == "") {
		return fmt.Errorf("cannot identify disposable image inputs for %s: %v", b.instance.Unique(), cause)
	}
	return nil
}

func (b *Builder) imageForExecution(recipe *builderv0.DockerBuildRecipe, identity imageIdentity, identityErr error) (*builderv0.DockerBuildRecipe, string, error) {
	if b.world.disposableImageScope != "" {
		return recipe, recipe.GetImage(), nil
	}
	if !b.localRuntime {
		return recipe, "", nil
	}
	name, err := localRuntimeImageName(recipe, identity, identityErr)
	if err != nil {
		return nil, "", err
	}
	execution := proto.CloneOf(recipe)
	execution.Image = name
	return execution, name, nil
}

func (b *Builder) cacheImageReferenceMismatch(entry *imageCacheEntry, recipe *builderv0.DockerBuildRecipe) bool {
	return (b.localRuntime || b.world.disposableImageScope != "") && entry.Image != recipe.GetImage()
}

func (b *Builder) checkDisposableImageDecision(recipe *builderv0.DockerBuildRecipe, reuse *imageReuse) error {
	if b.world.disposableImageScope == "" {
		return nil
	}
	if reuse.identity.Key == "" || reuse.image != recipe.GetImage() || (reuse.entry != nil && reuse.entry.Image != recipe.GetImage()) {
		return fmt.Errorf("disposable image execution does not match its admitted identity and reference")
	}
	return nil
}
