// Package imagedigest reads the registry digest the local Docker daemon
// recorded for an image: the manifest it pulled the image as, or pushed it as.
//
// A tag names whatever a registry points it at now; this digest names the
// bytes this machine actually holds, so it is what a record of "which image
// ran" or "which image was deployed" carries.
package imagedigest

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Local returns the sha256 manifest digest of image as the daemon holds it,
// read from its RepoDigests. An image that was never pulled from or pushed to
// a registry has none, and one the daemon knows under two different digests
// is ambiguous: both are errors, never a guess.
func Local(ctx context.Context, image string) (string, error) {
	output, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", image).Output()
	if err != nil {
		return "", fmt.Errorf("inspect %s repository digests: %w", image, err)
	}
	var references []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(output))), &references); err != nil {
		return "", fmt.Errorf("decode %s repository digests: %w", image, err)
	}
	return fromRepoDigests(image, references)
}

func fromRepoDigests(image string, references []string) (string, error) {
	var digest string
	for _, reference := range references {
		_, candidate, ok := strings.Cut(reference, "@")
		if !ok || !sha256Digest.MatchString(candidate) {
			continue
		}
		if digest != "" && digest != candidate {
			return "", fmt.Errorf("image %s resolves to multiple repository digests", image)
		}
		digest = candidate
	}
	if digest == "" {
		return "", fmt.Errorf("image %s has no registry-backed sha256 digest: the daemon holds it only as a local build; push the local snapshot first", image)
	}
	return digest, nil
}
