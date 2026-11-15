// Package imagedigest answers which manifest digest names an image: the one
// the local Docker daemon holds it under, and the one its registry serves.
//
// The two are different questions. On Docker's containerd image store an
// image that was never pushed already has a repository digest, its local
// manifest's; only the registry says what a tag names for everyone else. A
// record of "which image ran" or "which image was deployed" carries a digest
// the registry serves and the daemon's image matches (Published).
package imagedigest

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"github.com/distribution/reference"
)

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Local returns the sha256 manifest digest the daemon holds image under, in
// image's own repository. It is a local fact: on the containerd image store a
// build that was never pushed has one. A digest under another repository (the
// same image tagged twice) is not image's, and two different digests are
// ambiguous: both are errors, never a guess.
func Local(ctx context.Context, image string) (string, error) {
	named, references, err := repositoryDigests(ctx, image)
	if err != nil {
		return "", err
	}
	digests := digestsIn(named, references)
	switch len(digests) {
	case 0:
		return "", fmt.Errorf("image %s has no sha256 repository digest in %s", image, named.Name())
	case 1:
		return digests[0], nil
	default:
		return "", fmt.Errorf("image %s resolves to multiple repository digests", image)
	}
}

// Published returns the digest image's tag names at its registry, when the
// image the daemon holds is that one: the registry's digest is among the
// daemon's repository digests for the same repository. A local build the
// registry does not serve, or one built again after the push, is refused.
func Published(ctx context.Context, image string) (string, error) {
	named, references, err := repositoryDigests(ctx, image)
	if err != nil {
		return "", err
	}
	registry, err := Registry(ctx, image)
	if err != nil {
		return "", err
	}
	return published(image, named, registry, references)
}

func published(image string, named reference.Named, registry string, references []string) (string, error) {
	for _, digest := range digestsIn(named, references) {
		if digest == registry {
			return registry, nil
		}
	}
	return "", fmt.Errorf("the image the daemon holds as %s is not the one its registry serves (%s): it was built or tagged here and never pushed as it is", image, registry)
}

// repositoryDigests reads the daemon's RepoDigests for image.
func repositoryDigests(ctx context.Context, image string) (reference.Named, []string, error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return nil, nil, fmt.Errorf("parse image reference %s: %w", image, err)
	}
	output, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", image).Output()
	if err != nil {
		return nil, nil, fmt.Errorf("inspect %s repository digests: %w", image, err)
	}
	var references []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(output))), &references); err != nil {
		return nil, nil, fmt.Errorf("decode %s repository digests: %w", image, err)
	}
	return named, references, nil
}

// digestsIn is the distinct sha256 digests references hold in named's
// repository, compared as docker normalises names.
func digestsIn(named reference.Named, references []string) []string {
	var digests []string
	for _, ref := range references {
		parsed, err := reference.ParseNormalizedNamed(ref)
		if err != nil || parsed.Name() != named.Name() {
			continue
		}
		canonical, ok := parsed.(reference.Canonical)
		if !ok || !sha256Digest.MatchString(canonical.Digest().String()) {
			continue
		}
		if digest := canonical.Digest().String(); !slices.Contains(digests, digest) {
			digests = append(digests, digest)
		}
	}
	return digests
}
