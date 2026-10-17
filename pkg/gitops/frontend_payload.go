package gitops

import (
	"fmt"
	"slices"

	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/solutionhost"
)

// FrontendArtifactName is the name a release declares its component's
// complete frontend bundle under, as a runtime artifact of the package
// manifest: the one artifact a module manifest's generated frontend.payload names.
const FrontendArtifactName = "frontend"

// stampFrontendPayload generates the manifest's frontend.payload from the
// release's own artifact metadata, as core asks of a renderer: the runtime
// artifact named frontend gives the payload its artifact name, digest and
// media type, verbatim. A release declaring no such artifact gives a frontend
// no payload — a generation whose bundle is not published, which core admits
// and the host decides whether to serve. The renderer invents nothing: a
// bundle declared under another purpose, without the explicit media type the
// payload carries, or by a headless component (a bundle is the executable
// output of a declared page) is refused by name. That a release declares the
// name once is the manifest loader's rule.
func stampFrontendPayload(instance *ModuleInstance, artifacts []composition.ReleaseArtifact) error {
	index := slices.IndexFunc(artifacts, func(artifact composition.ReleaseArtifact) bool { return artifact.Name == FrontendArtifactName })
	if index < 0 {
		return nil
	}
	bundle := &artifacts[index]
	switch {
	case bundle.Purpose != composition.ArtifactRuntime:
		return fmt.Errorf("%s %s declares the release artifact %q with purpose %q; a frontend bundle is a runtime artifact", instance.Kind, instance.Name, FrontendArtifactName, bundle.Purpose)
	case instance.Frontend == nil:
		return fmt.Errorf("%s %s declares the release artifact %q and no frontend; a bundle is the output of a declared page", instance.Kind, instance.Name, FrontendArtifactName)
	case bundle.MediaType == "":
		return fmt.Errorf("%s %s declares the release artifact %q without a media type; the frontend payload carries the explicit canonical media type the release declares", instance.Kind, instance.Name, FrontendArtifactName)
	}
	instance.Frontend.Payload = &solutionhost.FrontendPayload{Artifact: bundle.Name, Digest: solutionhost.PayloadDigest(bundle.Digest), MediaType: bundle.MediaType}
	return nil
}
