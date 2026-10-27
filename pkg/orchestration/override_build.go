package orchestration

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// A released version names one build: the one made with the agent the release
// itself pins. A workspace's agent-overrides block moves the agent a composed
// service runs on without moving the module, and the agent emits the build
// recipe, so the same release built under an override is another build. It is
// never pushed under the release's tag — where it would overwrite the release's
// build, or be refused as a release whose version did not move with its code —
// but under a tag qualified by its own input identity, which no other build can
// claim. The cell pins every image by digest, so the tag a deployment names is
// informational; what the qualification protects is the registry's release tag.

// buildAgentLabel records the agent that emitted an image's build recipe, so a
// refusal can say whether the agent is what differs from a published build.
const buildAgentLabel = "org.codefly.build.agent"

// overrideBuildTagInfix separates the release version from the identity in a
// build-qualified tag: <version>-build.<identity>. Semver reads it as a
// prerelease of the version, which is what releasedVersionTag refuses to
// treat as a release.
const overrideBuildTagInfix = "-build."

// overrideBuildIdentityLength is how much of the input identity a qualified tag
// carries: 64 bits, far past any chance of two builds of one version colliding,
// and a published image's inputs label is still checked before it is adopted.
const overrideBuildIdentityLength = 16

// overriddenAgent reports the agent version the service's own module pins when
// the workspace's agent-overrides moved it to another one, and "" when the
// service builds with its module's pin. Core applies an override when a
// composed module loads the service (resources.Module.LoadServiceFromReference)
// and keeps no record of what it replaced; the service's own file, loaded on
// its own, is that record (as composition.ResolveAgentOverrides reads it).
func (b *Builder) overriddenAgent(ctx context.Context) (string, error) {
	service := b.instance.Service
	if b.world == nil || b.world.Workspace == nil || service == nil || service.Agent == nil {
		return "", nil
	}
	overrides, err := b.world.Workspace.AgentOverrides()
	if err != nil {
		return "", err
	}
	matched := false
	for _, override := range overrides {
		matched = matched || override.Matches(service.Agent)
	}
	if !matched {
		return "", nil
	}
	declared, err := resources.LoadServiceFromDir(ctx, service.Dir())
	if err != nil {
		return "", fmt.Errorf("cannot read the agent %s pins: %w", b.instance.Unique(), err)
	}
	if declared.Agent == nil || declared.Agent.Version == service.Agent.Version {
		return "", nil
	}
	return declared.Agent.Version, nil
}

// overrideBuildImage is image with its release tag qualified by the build's
// input identity: example.test/app:1.2.3 → example.test/app:1.2.3-build.<16 hex>.
func overrideBuildImage(image, identityKey string) string {
	identity := strings.TrimPrefix(identityKey, "sha256:")
	if len(identity) > overrideBuildIdentityLength {
		identity = identity[:overrideBuildIdentityLength]
	}
	return image + overrideBuildTagInfix + identity
}

// withBuildAgentLabel records the agent that emitted the recipe on the image.
// Like the inputs label it is added after the identity was computed.
func withBuildAgentLabel(args []string, agent *resources.Agent) []string {
	if agent == nil {
		return args
	}
	return withBuildFlags(args, "--label", buildAgentLabel+"="+agent.Identifier())
}

// publishedFromOtherInputs is the refusal for a released tag that holds a build
// of other inputs, naming which class of input differs when the published image
// says: the agent that emitted its recipe, or — the same agent — the service's
// code or a base image.
func publishedFromOtherInputs(image string, agent *resources.Agent, publishedAgent string) error {
	const release = "release a new version (codefly publish) instead of rebuilding a published one"
	switch {
	case agent != nil && publishedAgent != "" && publishedAgent != agent.Identifier():
		return fmt.Errorf("%s is already published from a build by agent %s, and this build runs %s, the agent the release pins: "+
			"the tag holds a build the release does not name (made under a workspace agent override by a CLI that published it under the release tag); "+
			"%s, or remove that tag from the registry", image, publishedAgent, agent.Identifier(), release)
	case publishedAgent != "":
		return fmt.Errorf("%s is already published from other inputs by the same agent %s: the service's code or a base image changed without its version moving; %s",
			image, publishedAgent, release)
	default:
		return fmt.Errorf("%s is already published from other inputs: the service's code, its agent's build recipe or a base image differ, and the published image does not record which; %s",
			image, release)
	}
}
