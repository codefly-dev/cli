package orchestration

import (
	"context"

	coresbom "github.com/codefly-dev/core/agents/services/sbom"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/wool"
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
		expected = producedSubjects(service, plan, b.world.Push)
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
	if err := coresbom.ValidateCoverage(expected, response); err != nil {
		return w.Wrapf(err, "image SBOM coverage for %s", service)
	}
	b.imageEvidence = response.GetImages()
	return nil
}

// producedSubjects narrows the recipe's declared coverage to the images this
// build actually made. A push writes every declared platform into one manifest
// list, so all of them are owed evidence. A local build loads a single platform
// per recipe — the same first one buildxArgs selects — and demanding evidence
// for the rest fails the build over images that were never built.
func producedSubjects(service string, plan *builderv0.DockerBuildPlan, push bool) []*builderv0.ImageSubject {
	declared := coresbom.ExpectedFromBuildPlan(service, plan)
	if push {
		return declared
	}
	loaded := map[string]string{}
	for _, recipe := range plan.GetRecipes() {
		if platforms := recipe.GetPlatforms(); len(platforms) > 0 {
			loaded[recipe.GetName()] = platforms[0]
		}
	}
	var produced []*builderv0.ImageSubject
	for _, subject := range declared {
		if platform, stated := loaded[subject.GetRole()]; !stated || subject.GetPlatform() == platform {
			produced = append(produced, subject)
		}
	}
	return produced
}
