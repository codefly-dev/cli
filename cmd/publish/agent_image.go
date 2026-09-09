package publish

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/core/resources"
)

// agentImage is the runtime image an agent repo publishes. The repo's whole
// contract is a checked-in Dockerfile plus the version in
// agent.codefly.yaml: publish derives the reference through
// resources.PublishedImage, builds, pushes, and verifies it is anonymously
// pullable. No agent repo's CI names a registry, holds a registry
// credential, or hand-computes a digest — a registry move is a change here
// and in core, not in every agent repo's workflow YAML.
type agentImage struct {
	// dir is the agent repo root: both the Dockerfile's location and the
	// build context.
	dir string
	// name is the registry package — <kind>-<agent> (service-redis,
	// toolbox-web), matching the GitHub repository the ghcr package is
	// linked to.
	name string
}

// agentImagePlatforms are the platforms every published agent runtime image
// carries. Agents run in containers on linux/amd64 hosts and on
// Apple-silicon workstations; publishing whatever architecture the
// operator's laptop happens to be would leave one of them unable to pull.
var agentImagePlatforms = []string{"linux/amd64", "linux/arm64"}

// hasDockerfile reports whether the agent repo hands publish a Dockerfile
// to build. Absence is how an agent that ships no runtime image opts out.
func hasDockerfile(agentDir string) bool {
	_, err := os.Stat(filepath.Join(agentDir, "Dockerfile"))
	return err == nil
}

// newAgentImage returns the runtime image agentDir publishes, or nil when
// the repo checks in no Dockerfile.
func newAgentImage(agentDir, kind, name string) (*agentImage, error) {
	if !hasDockerfile(agentDir) {
		return nil, nil
	}
	reg, err := agentRegistration(kind)
	if err != nil {
		return nil, err
	}
	return &agentImage{dir: agentDir, name: reg.GitHubRepository(name)}, nil
}

// reference is the image reference this repo publishes at version,
// addressed through resources.ImageRegistry.
func (i *agentImage) reference(version string) string {
	image := resources.PublishedImage(i.name, version)
	return image.FullName()
}

// buildxArgs is the buildx invocation for one phase of the flow. A
// multi-platform result cannot be loaded into the local daemon, so the
// pre-tag validation build produces no artifact (type=cacheonly) and the
// post-tag push rebuilds straight from that cache.
func (i *agentImage) buildxArgs(version string, push bool) []string {
	args := []string{
		"buildx", "build",
		"--platform", strings.Join(agentImagePlatforms, ","),
		"-f", "Dockerfile",
		"-t", i.reference(version),
	}
	if push {
		args = append(args, "--push")
	} else {
		args = append(args, "--output", "type=cacheonly")
	}
	return append(args, ".")
}

// build compiles the checked-in Dockerfile for every published platform
// without producing an artifact. It runs before any git mutation, so a
// Dockerfile that doesn't build aborts the release with nothing committed,
// tagged, or pushed.
func (i *agentImage) build(ctx context.Context, version string) error {
	ref := i.reference(version)
	fmt.Printf("==> Building runtime image %s (%s)\n", ref, strings.Join(agentImagePlatforms, ", "))
	if err := i.docker(ctx, i.buildxArgs(version, false)); err != nil {
		return fmt.Errorf("build runtime image %s: %w", ref, err)
	}
	return nil
}

// publish pushes the multi-platform manifest and then fails unless the
// reference is anonymously pullable — the same check `codefly companion
// verify` runs, so a push that only succeeded against the operator's own
// credentials cannot leave the package private for every consumer.
func (i *agentImage) publish(ctx context.Context, version string) error {
	ref := i.reference(version)
	fmt.Printf("==> Publishing runtime image %s to %s\n", ref, builder.RegistryHost(ref))
	if err := i.docker(ctx, i.buildxArgs(version, true)); err != nil {
		return fmt.Errorf("push runtime image %s: %w", ref, err)
	}
	if err := builder.VerifyAnonymouslyPullable(i.name, ref); err != nil {
		return err
	}
	fmt.Printf("    published %s\n", ref)
	return nil
}

func (i *agentImage) docker(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = i.dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// checkAgentImagePreconditions rejects a host that cannot build the
// Dockerfile the repo checked in. Deterministic and side-effect free, so
// `publish all` can reject the host during its validate phase rather than
// after release-grade CI has already run — and, for a single publish,
// before the tag is cut.
func checkAgentImagePreconditions(agentDir string) error {
	if !hasDockerfile(agentDir) {
		return nil
	}
	if err := exec.Command("docker", "buildx", "version").Run(); err != nil {
		return fmt.Errorf(
			"%s checks in a Dockerfile, so publish builds and pushes its runtime image for %s, but `docker buildx` is unavailable: %w",
			agentDir, strings.Join(agentImagePlatforms, ", "), err)
	}
	return nil
}
