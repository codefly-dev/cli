package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/asottile/dockerfile"
	coreservices "github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/services"
	"google.golang.org/protobuf/proto"
)

func (b *Builder) selectBuildImages(ctx context.Context) error {
	b.imageBindings = nil
	if b.world.Env == nil {
		return nil
	}
	policy := b.world.Env.BuildImages
	agent := b.instance.Service.Agent
	agentType := agent.Publisher + "/" + agent.Name
	if !policy.Applies(agentType, b.instance.Unique()) {
		return nil
	}
	client, err := services.LoadBuilder(ctx, b.instance.Service)
	if err != nil {
		return err
	}
	capabilities, err := client.BuildCapabilities(ctx, &builderv0.BuildCapabilitiesRequest{})
	if err != nil {
		return fmt.Errorf("cannot inspect image inputs for %s: %w", b.instance.Unique(), err)
	}
	if err = coreservices.RequireBuildImageBindings(capabilities); err != nil {
		return err
	}
	b.imageBindings, err = policy.Resolve(agentType, b.instance.Unique(), capabilities.GetImageInputs())
	return err
}

// verifyBuildImages closes the producer/consumer boundary: a new getter or a
// claimed capability cannot silently discard environment policy. It runs after
// normal recipe integrity checks, before any prefetch or image execution.
func verifyBuildImages(outputDir string, plan *builderv0.DockerBuildPlan, bindings []*builderv0.BuildImageInput) error {
	if len(bindings) == 0 {
		return nil
	}
	if plan == nil {
		return fmt.Errorf("image bindings require a verified Docker build plan")
	}
	expected := map[string]bool{}
	for _, input := range bindings {
		if input.Reference != "scratch" {
			expected[input.Reference] = false
		}
	}
	for _, recipe := range plan.GetRecipes() {
		path, err := recipeDockerfile(outputDir, recipe)
		if err != nil {
			return err
		}
		refs, err := externalPolicyImages(path)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			if _, ok := expected[ref]; !ok {
				return fmt.Errorf("recipe %q uses undeclared or unselected image %q", recipe.GetName(), ref)
			}
			expected[ref] = true
		}
	}
	for ref, used := range expected {
		if !used {
			return fmt.Errorf("recipe omitted selected image %q", ref)
		}
	}
	return nil
}

// externalPolicyImages covers external COPY and RUN mounts and the frontend as
// well as FROM. Parameters whose image cannot be proven refuse policy execution.
func externalPolicyImages(path string) ([]string, error) {
	commands, err := dockerfile.ParseFile(path)
	if err != nil {
		return nil, err
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	refs := []string{}
	stages := map[string]bool{"scratch": true}
	add := func(ref string) error {
		if stages[strings.ToLower(ref)] {
			return nil
		}
		if ref == "" || strings.ContainsAny(ref, "$") {
			return fmt.Errorf("unresolved external image %q in %s", ref, path)
		}
		refs = append(refs, ref)
		return nil
	}
	// Docker parser directives precede the first non-comment instruction.
	for _, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			break
		}
		directive := strings.TrimSpace(strings.TrimPrefix(line, "#"))
		if key, value, ok := strings.Cut(directive, "="); ok && strings.EqualFold(strings.TrimSpace(key), "syntax") {
			if err = add(strings.TrimSpace(value)); err != nil {
				return nil, err
			}
		}
	}
	stageIndex := 0
	for _, command := range commands {
		if strings.EqualFold(command.Cmd, "from") {
			if len(command.Value) == 0 {
				return nil, fmt.Errorf("FROM has no image in %s", path)
			}
			if err = add(command.Value[0]); err != nil {
				return nil, err
			}
			stages[fmt.Sprint(stageIndex)] = true
			stageIndex++
			if len(command.Value) >= 3 && strings.EqualFold(command.Value[1], "as") {
				stages[strings.ToLower(command.Value[2])] = true
			}
		}
		if strings.EqualFold(command.Cmd, "copy") || strings.EqualFold(command.Cmd, "run") {
			for _, flag := range command.Flags {
				if value, ok := strings.CutPrefix(flag, "--from="); ok {
					if err = add(value); err != nil {
						return nil, err
					}
				}
				if value, ok := strings.CutPrefix(flag, "--mount="); ok {
					for _, option := range strings.Split(value, ",") {
						if from, ok := strings.CutPrefix(option, "from="); ok {
							if err = add(from); err != nil {
								return nil, err
							}
						}
					}
				}
			}
		}
	}
	return refs, nil
}

func withImageBindingsLabel(args []string, bindings []*builderv0.BuildImageInput) []string {
	if len(bindings) == 0 {
		return args
	}
	payload, _ := json.Marshal(bindings) // protobuf fields contain only strings
	return withBuildFlags(args, "--label", "org.codefly.build.image-bindings="+string(payload))
}

// qualifyBoundImage keeps environment-specific inputs apart from owner releases.
func qualifyBoundImage(recipe *builderv0.DockerBuildRecipe, decision imageReuse, identityErr error, shouldPush bool, bindings []*builderv0.BuildImageInput) (*builderv0.DockerBuildRecipe, imageReuse, error) {
	if len(bindings) == 0 {
		return recipe, decision, nil
	}
	if decision.identity.Key == "" {
		return recipe, decision, fmt.Errorf("selected image bindings require a complete build identity: %w", identityErr)
	}
	if shouldPush && decision.image == "" {
		recipe = proto.CloneOf(recipe)
		recipe.Image = overrideBuildImage(recipe.GetImage(), decision.identity.Key)
		decision.image = recipe.Image
	}
	return recipe, decision, nil
}

func (b *Builder) verifyImagePlan(outputDir string, plan *builderv0.DockerBuildPlan) error {
	if plan != nil {
		if err := coreservices.VerifyDockerBuildPlan(outputDir, plan); err != nil {
			return fmt.Errorf("cannot verify build recipe for %s: %w", b.instance.Unique(), err)
		}
	}
	if err := verifyBuildImages(outputDir, plan, b.imageBindings); err != nil {
		return err
	}
	if plan != nil {
		return b.checkDisposableImagePlan(plan)
	}
	return nil
}
