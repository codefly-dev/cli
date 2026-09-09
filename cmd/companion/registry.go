package companion

import (
	"fmt"
	"strings"
)

// Registry-reference helpers shared by the push and verify paths. They
// derive everything from the image reference itself, so no caller has to
// pass a name, org or namespace alongside a tag that already contains one.

// registryHost extracts the registry host a tag will push to, for status
// messages and login hints. Following Docker's own reference resolution: the
// first path segment is a host only when it contains "." or ":" or is
// "localhost" — otherwise the image is implicitly under docker.io.
func registryHost(tag string) string {
	if i := strings.IndexByte(tag, '/'); i >= 0 {
		first := tag[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			return first
		}
	}
	return "docker.io"
}

// registryPrivacyHint returns registry-appropriate instructions for making
// a pushed image publicly accessible. Everything it needs is derived from the
// reference itself — taking the org or namespace from a separately passed-in
// name emits a codefly-dev URL for a tag pushed to any other org.
func registryPrivacyHint(tag string) string {
	repo := repoPath(tag)
	switch registryHost(tag) {
	case "ghcr.io":
		org, pkg, found := strings.Cut(strings.TrimPrefix(repo, "ghcr.io/"), "/")
		if !found {
			break
		}
		return fmt.Sprintf("make it public at https://github.com/orgs/%s/packages/container/%s/settings", org, pkg)
	case "docker.io":
		return fmt.Sprintf("make it public at https://hub.docker.com/repository/docker/%s/general", repo)
	}
	return fmt.Sprintf("check %s's visibility settings in its registry", tag)
}

// repoPath strips the ":tag" or "@digest" suffix from an image reference,
// leaving the repository path registry URLs are keyed on. A colon before the
// last "/" is a registry port rather than a tag separator, and a digest's own
// "sha256:" colon must not be read as one either.
func repoPath(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		ref = ref[:i]
	}
	return ref
}

// registryLoginHint returns the login command for the registry a tag targets.
// The credential is registry-specific: ghcr.io accepts a GitHub token, so the
// exact command can be named. Anywhere else a GitHub token is meaningless, and
// printing one sends the operator into a login that cannot succeed.
func registryLoginHint(tag string) string {
	host := registryHost(tag)
	if host == "ghcr.io" {
		return fmt.Sprintf("docker login %s -u <user> -p $(gh auth token)", host)
	}
	return fmt.Sprintf("docker login %s", host)
}
