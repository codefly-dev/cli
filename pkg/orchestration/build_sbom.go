package orchestration

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	coresbom "github.com/codefly-dev/core/agents/services/sbom"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/wool"
	"google.golang.org/protobuf/proto"
)

// collectImageEvidence requires a digest-bound CycloneDX inventory for every
// image this build produced. The expectation is derived from the service's own
// recipes or build result, so a service that adds a platform or an image is
// covered without a list of known services to keep in sync.
//
// A service that emits neither is asked with no subjects: the agent then has to
// declare a no-image reason, which keeps "this service ships nothing" distinct
// from "nobody implemented this" instead of both reading as success.
func (b *Builder) collectImageEvidence(ctx context.Context, plan *builderv0.DockerBuildPlan, result *builderv0.DockerBuildResult) error {
	w := wool.Get(ctx).In("Builder.collectImageEvidence", wool.ThisField(b.instance))
	service := b.instance.Unique()

	var expected []*builderv0.ImageSubject
	switch {
	case plan != nil:
		var err error
		if expected, err = producedSubjects(service, plan, b.world.Push, b.resolvedImages); err != nil {
			return w.Wrapf(err, "cannot derive image SBOM coverage for %s", service)
		}
	case result != nil:
		expected = coresbom.ExpectedFromBuildResult(service, result)
	}

	response, err := b.instance.Builder.SBOM(ctx, &builderv0.SBOMRequest{
		Scope:    builderv0.SBOMScope_SBOM_SCOPE_IMAGE,
		Subjects: expected,
	})
	if err != nil {
		return w.Wrapf(err, "cannot collect image SBOM for %s", service)
	}
	if err := coresbom.ValidateCoverage(service, expected, response); err != nil {
		return w.Wrapf(err, "image SBOM coverage for %s", service)
	}
	b.imageEvidence = response.GetImages()
	return nil
}

// producedSubjects derives the coverage owed by the images this build actually
// made, each pinned to the identity the build resolved for it. A subject
// carrying only a tag would bind evidence to whatever that tag serves rather
// than to the image that was built, so a recipe the build resolved nothing for
// is refused here rather than answered with evidence about something else.
func producedSubjects(service string, plan *builderv0.DockerBuildPlan, push bool, resolved []coresbom.ResolvedImage) ([]*builderv0.ImageSubject, error) {
	return coresbom.ExpectedFromBuildPlan(service, producedPlan(plan, push), resolved)
}

// producedPlan restricts the plan to what buildx was asked to produce. A push
// writes every declared platform into one manifest list, so all of them are
// owed evidence. A local build loads a single platform per recipe — the same
// first one buildxArgs selects — and demanding evidence for the rest fails the
// build over images that were never built.
func producedPlan(plan *builderv0.DockerBuildPlan, push bool) *builderv0.DockerBuildPlan {
	if push {
		return plan
	}
	recipes := make([]*builderv0.DockerBuildRecipe, 0, len(plan.GetRecipes()))
	for _, recipe := range plan.GetRecipes() {
		if len(recipe.GetPlatforms()) > 1 {
			loaded, _ := proto.Clone(recipe).(*builderv0.DockerBuildRecipe)
			loaded.Platforms = recipe.GetPlatforms()[:1]
			recipe = loaded
		}
		recipes = append(recipes, recipe)
	}
	return &builderv0.DockerBuildPlan{Recipes: recipes}
}

// recordPushedImage binds every platform of a pushed recipe to the manifest
// list the push produced: buildx writes all of them into one index, whose
// digest is the identity a scan resolves each platform's image out of.
func (b *Builder) recordPushedImage(recipe *builderv0.DockerBuildRecipe, digest string) {
	for _, platform := range recipePlatforms(recipe) {
		b.resolvedImages = append(b.resolvedImages, coresbom.ResolvedImage{
			Recipe:   recipe.GetName(),
			Platform: platform,
			Digest:   digest,
		})
	}
}

// recordLoadedImage binds the one platform a local build loads to the image ID
// the daemon holds. A never-pushed image has no registry manifest, so that ID
// is the only immutable identity evidence about it can be bound to.
func (b *Builder) recordLoadedImage(recipe *builderv0.DockerBuildRecipe, imageID string) {
	b.resolvedImages = append(b.resolvedImages, coresbom.ResolvedImage{
		Recipe:   recipe.GetName(),
		Platform: recipePlatforms(recipe)[0],
		Digest:   imageID,
		Source:   coresbom.SourceDockerDaemon,
	})
}

// recipePlatforms is the recipe's declared platforms, or the single unnamed one
// a recipe that declares none is built for.
func recipePlatforms(recipe *builderv0.DockerBuildRecipe) []string {
	if platforms := recipe.GetPlatforms(); len(platforms) > 0 {
		return platforms
	}
	return []string{""}
}

// inspectLocalImageID resolves the image ID the daemon holds for a loaded
// image. inspectImageDigest reads RepoDigests, which an image that was never
// pushed does not have.
func inspectLocalImageID(ctx context.Context, image string) (string, error) {
	output, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image).Output()
	if err != nil {
		return "", fmt.Errorf("inspect %s image id: %w", image, err)
	}
	id := strings.TrimSpace(string(output))
	if !sha256Digest.MatchString(id) {
		return "", fmt.Errorf("image %s resolves to %q, which is not a sha256 image id", image, id)
	}
	return id, nil
}
