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
		expected = coresbom.ExpectedFromBuildPlan(service, plan)
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
