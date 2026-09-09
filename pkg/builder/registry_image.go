package builder

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// This file holds the push/verify half of codefly's own artifact registry —
// the registry codefly PUBLISHES to (companion images, agent runtime
// images), addressed through resources.ImageRegistry. It is deliberately
// independent of the workspace registry state in this package (the
// `repository` global, RegistryLogin's env.Registry auth): that one pushes a
// user's service images to a user-owned registry with the user's cloud
// credentials, this one publishes codefly's images to a registry codefly
// owns and every consumer pulls anonymously. Co-located, never shared.

// pushVerifyAttempts/pushVerifyDelay control the retry of the post-push
// anonymous pull check below. A tag that was just pushed can take a moment
// to become anonymously readable (registry propagation, or — per Docker
// Hub's documented behavior for a namespaced repo — a freshly written tag
// occasionally reporting an auth-shaped error for the first read or two),
// so a single failed check right after push is not yet evidence the
// package is actually private. Package vars so tests can shrink the delay.
var (
	pushVerifyAttempts = 3
	pushVerifyDelay    = time.Second
)

// PushImage runs `docker push <ref>`, then re-checks the reference
// anonymously (see VerifyAnonymouslyPullable). name is the image's registry
// package name, used only to build the visibility hint.
func PushImage(name, ref string) error {
	host := RegistryHost(ref)
	fmt.Printf("    pushing %s to %s\n", ref, host)

	out, runErr := exec.Command("docker", "push", ref).CombinedOutput()
	os.Stdout.Write(out)
	if runErr != nil {
		if isPushDenied(string(out)) {
			return fmt.Errorf("docker push %s failed: not authenticated for %s\nfix: docker login %s -u <user> -p $(gh auth token)", ref, host, host)
		}
		return fmt.Errorf("docker push %s failed: %w", ref, runErr)
	}

	return VerifyAnonymouslyPullable(name, ref)
}

// VerifyAnonymouslyPullable fails unless ref resolves with no stored
// credentials, so a push that only succeeded because the operator's local
// daemon is logged in doesn't silently leave the package private for
// everyone else. Callers that push through buildx (which publishes the
// manifest itself, so there is no separate `docker push` to wrap) run this
// directly; PushImage runs it for them.
func VerifyAnonymouslyPullable(name, ref string) error {
	ok, out, err := anonymousManifestInspectRetrying(ref)
	if err != nil {
		return fmt.Errorf("push %s succeeded but the anonymous pull check could not run: %w", ref, err)
	}
	if !ok {
		return fmt.Errorf(`push %s succeeded but is not publicly pullable: %s
fix: %s`,
			ref, strings.TrimSpace(out), RegistryPrivacyHint(name, ref))
	}
	return nil
}

// anonymousManifestInspectRetrying retries AnonymousManifestInspect up to
// pushVerifyAttempts times, pausing pushVerifyDelay between attempts, and
// returns as soon as a check succeeds, errors outright, or the attempts run
// out — so a transient just-pushed-not-yet-readable response isn't reported
// as a permanently private package.
func anonymousManifestInspectRetrying(ref string) (ok bool, output string, err error) {
	for attempt := 1; ; attempt++ {
		ok, output, err = AnonymousManifestInspect(ref)
		if err != nil || ok || attempt >= pushVerifyAttempts {
			return ok, output, err
		}
		time.Sleep(pushVerifyDelay)
	}
}

// AnonymousManifestInspect runs `docker manifest inspect <ref>` with
// DOCKER_CONFIG pointed at a throwaway directory holding an empty config,
// so the lookup carries no stored credentials regardless of what the
// operator is logged into locally. ok reports whether the command
// succeeded; output is its combined stdout+stderr for callers to classify
// (IsManifestNotFound) or report. err is only set for failures unrelated
// to the docker command's own exit status (e.g. can't create the temp
// config dir).
func AnonymousManifestInspect(ref string) (ok bool, output string, err error) {
	configDir, err := os.MkdirTemp("", "codefly-anon-docker-*")
	if err != nil {
		return false, "", fmt.Errorf("create anonymous docker config dir: %w", err)
	}
	defer os.RemoveAll(configDir)
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte("{}"), 0o600); err != nil {
		return false, "", fmt.Errorf("write anonymous docker config: %w", err)
	}

	cmd := exec.Command("docker", "manifest", "inspect", ref)
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	out, runErr := cmd.CombinedOutput()
	return runErr == nil, string(out), nil
}

// IsManifestNotFound classifies `docker manifest inspect` failure output as
// "the reference isn't in the registry" versus some other failure. It
// matches only the two ways docker and the registry v2 API phrase an absent
// manifest — a broader match (e.g. bare "not found") would swallow
// repository/auth errors and let a verification pass when it shouldn't.
func IsManifestNotFound(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "no such manifest") ||
		strings.Contains(lower, "manifest unknown")
}

// RegistryHost extracts the registry host a reference will push to, for
// status messages and login hints. Following Docker's own reference
// resolution: the first path segment is a host only when it contains "." or
// ":" or is "localhost" — otherwise the image is implicitly under docker.io.
func RegistryHost(ref string) string {
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		first := ref[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			return first
		}
	}
	return "docker.io"
}

// RegistryPrivacyHint returns registry-appropriate instructions for making
// a pushed image publicly accessible. Everything codefly publishes lives
// under resources.ImageRegistry (ghcr.io), but callers also push references
// they were handed directly, so the hint matches the reference's actual
// registry rather than assuming ghcr.io.
func RegistryPrivacyHint(name, ref string) string {
	switch RegistryHost(ref) {
	case "ghcr.io":
		return fmt.Sprintf("make it public at https://github.com/orgs/codefly-dev/packages/container/%s/settings", name)
	case "docker.io":
		return fmt.Sprintf("make it public at https://hub.docker.com/repository/docker/codeflydev/%s/general", name)
	default:
		return fmt.Sprintf("check %s's visibility settings in its registry", ref)
	}
}

// isPushDenied reports whether `docker push` output indicates the daemon
// isn't authenticated for the target registry, as opposed to some other
// failure (network, bad reference, ...) that a login hint wouldn't fix.
func isPushDenied(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "denied") || strings.Contains(lower, "unauthorized")
}
