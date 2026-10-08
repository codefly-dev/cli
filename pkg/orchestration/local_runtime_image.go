package orchestration

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/agents/contract"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// localRuntimeImage runs before Runtime.Init, after the flow selected a backend.
// Stock-image agents may have a builder but emit no recipe. They keep their
// own image selection; no agent-name or release-version table is involved.
func (runner *Runner) localRuntimeImage(ctx context.Context) (string, error) {
	if runner.remoteEnvironment != nil || runner.runtimeContext != resources.RuntimeContextContainer {
		return "", nil
	}
	// An explicit manifest choice wins under the handoff contract. Leave its
	// validation to the agent, including refusal of malformed/latest values.
	if image, ok := runner.instance.Service.Spec["runtime-image"].(string); ok && image != "" {
		return "", nil
	}
	if err := runner.instance.CheckCapabilities(agentv0.Capability_BUILDER); err != nil {
		return "", nil
	}
	if advertised, supported := ValidationOperationSupport(runner.instance.Info, ValidationArtifactBuild); advertised && !supported {
		return "", nil
	}
	if runner.instance.Builder == nil {
		if err := runner.instance.LoadBuilder(ctx); err != nil {
			return "", err
		}
	}
	// Runtime.Load already published the endpoints. Loading the builder must
	// not overwrite those runtime declarations in shared state.
	runner.instance.Builder.Workspace = runner.world.Workspace
	if _, err := runner.instance.Builder.Load(ctx); err != nil {
		return "", err
	}
	b, err := NewBuilder(ctx, runner.instance, runner.world)
	if err != nil {
		return "", err
	}
	if _, err = b.Init(ctx); err != nil {
		return "", err
	}
	return b.buildLocalRuntimeImage(ctx)
}

// buildLocalRuntimeImage executes an agent's recipe through the same verified
// buildx path as deploy. Load and Init of the builder must have completed first.
// An empty result means the builder emits no recipe (for example a stock-image
// service); that agent continues to own its existing runtime image selection.
func (b *Builder) buildLocalRuntimeImage(ctx context.Context) (string, error) {
	if b.world.Push {
		return "", fmt.Errorf("local runtime images must be loaded into the container engine, not pushed")
	}
	b.localRuntime = true
	if _, err := b.Plan(ctx); err != nil {
		return "", err
	}
	planned := b.planned
	b.planned = nil
	plan := planned.response.GetResult().GetDockerBuildPlan()
	if plan == nil {
		return "", nil
	}
	if len(plan.GetRecipes()) != 1 {
		return "", fmt.Errorf("local runtime for %s requires exactly one image recipe, got %d", b.instance.Unique(), len(plan.GetRecipes()))
	}
	if err := b.instance.RequireAgentCapabilities(contract.RuntimeInitImage); err != nil {
		return "", fmt.Errorf("cannot supply a local runtime image to %s: %w", b.instance.Unique(), err)
	}
	if err := b.buildFromPlan(ctx, planned.outputDir, plan, planned.proxies); err != nil {
		return "", err
	}
	if err := recordBuildRecipe(ctx, b.instance.Service, planned.recipeRoot, plan); err != nil {
		return "", err
	}
	return plan.GetRecipes()[0].GetImage(), nil
}

func localRuntimeRecipeRoot(workspaceDir, module, service string) (string, error) {
	if workspaceDir == "" {
		return "", fmt.Errorf("a local runtime recipe requires a workspace directory")
	}
	for _, name := range []string{module, service} {
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
			return "", fmt.Errorf("invalid module/service identity for a local runtime recipe: %q", name)
		}
	}
	return filepath.Join(workspaceDir, ".codefly", "runtime-build", module, service), nil
}

// prepareLocalRuntimeRecipe runs after plan verification. The destination is
// normalized before hashing so the resulting tag is not its own input. The
// original verified plan digest (including agent metadata) remains bound.
func (b *Builder) prepareLocalRuntimeRecipe(recipe *builderv0.DockerBuildRecipe) error {
	if !b.localRuntime {
		return nil
	}
	host := b.world.hostArchitecture
	if host == nil {
		host = dockerEngineArchitecture
	}
	arch, err := host()
	if err != nil {
		return err
	}
	platform := "linux/" + arch
	if !recipeBuildsPlatform(recipe.GetPlatforms(), platform) {
		return fmt.Errorf("local container engine requires %s, which recipe %s does not build (it builds %v)", platform, recipe.GetName(), recipe.GetPlatforms())
	}
	recipe.Platforms = []string{platform}
	recipe.Image = "codefly-local/runtime:recipe-inputs"
	return nil
}

func (b *Builder) nameLocalRuntimeImage(recipe *builderv0.DockerBuildRecipe, identity imageIdentity, identityErr error) error {
	if !b.localRuntime {
		return nil
	}
	if identityErr != nil {
		return fmt.Errorf("cannot identify local runtime image inputs: %w", identityErr)
	}
	if identity.Key == "" {
		return fmt.Errorf("cannot identify local runtime image inputs for %s", recipe.GetName())
	}
	recipe.Image = "codefly-local/runtime:sha256-" + strings.TrimPrefix(identity.Key, "sha256:")
	return nil
}

// Unlike a deployment's mutable version tag, the local tag claims particular
// inputs. A build whose context changed cannot be handed to Runtime.Init under
// that claim, even if it succeeded. The next invocation will re-plan and build.
func (b *Builder) verifyLocalRuntimeBuild(ctx context.Context, w *wool.Wool, recipe *builderv0.DockerBuildRecipe, scope imageBuildScope, identity imageIdentity) error {
	if !b.localRuntime {
		return nil
	}
	if !b.contextHeldStill(ctx, w, recipe, scope, identity) {
		return fmt.Errorf("local runtime build inputs changed while building %s; retry the run", recipe.GetImage())
	}
	if _, err := inspectLocalImageID(ctx, recipe.GetImage()); err != nil {
		return fmt.Errorf("local runtime image was not loaded: %w", err)
	}
	return nil
}
