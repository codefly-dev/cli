package publish

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Masterminds/semver"
)

// devPrereleaseLabel starts the prerelease of every dev build: a build that is
// published for iteration and never released.
const devPrereleaseLabel = "dev"

// devSHALength is how many characters of the commit a dev version carries.
const devSHALength = 12

var (
	commitSHA        = regexp.MustCompile(`^[0-9a-f]{40}$`)
	devPrerelease    = regexp.MustCompile(`^` + devPrereleaseLabel + `\.[0-9a-f]{12}$`)
	allDigitsPattern = regexp.MustCompile(`^[0-9]+$`)
)

// DevVersion derives the non-semantic version a dev build of the agent at
// commit sha publishes under: `<base>-dev.<12-char-sha>`.
//
// It is a semver prerelease on purpose. Every resolver parses it, and semver
// ranks a prerelease BELOW its release — 0.1.47-dev.<sha> < 0.1.47 — so a dev
// build can never collide with or outrank the release it was built from, and
// GitHub's latest-release lookup (which excludes prereleases) never returns it.
//
// base must be a plain release version: a dev build of a beta would sort above
// the beta it came from, and so could outrank a real prerelease.
func DevVersion(base *semver.Version, sha string) (string, error) {
	if base == nil {
		return "", fmt.Errorf("dev version needs the agent's current version")
	}
	if base.Prerelease() != "" || base.Metadata() != "" {
		return "", fmt.Errorf("current version %s is not a plain release; a dev build is derived from a released X.Y.Z", base)
	}
	sha = strings.ToLower(strings.TrimSpace(sha))
	if !commitSHA.MatchString(sha) {
		return "", fmt.Errorf("dev version needs a full 40-character commit sha, got %q", sha)
	}
	short := sha[:devSHALength]
	// Strict semver forbids a leading zero in a numeric identifier, and a
	// hex prefix can be all digits. The strict parsers the loader uses would
	// then refuse the version outright, so refuse to publish it.
	if allDigitsPattern.MatchString(short) && short[0] == '0' {
		return "", fmt.Errorf("commit %s renders the all-numeric identifier %q, which strict semver rejects; make any new commit and publish again", sha, short)
	}
	return fmt.Sprintf("%d.%d.%d-%s.%s", base.Major(), base.Minor(), base.Patch(), devPrereleaseLabel, short), nil
}

// IsDevVersion reports whether version names a dev build — one published by
// `codefly publish dev` rather than released. A leading `v` is accepted.
func IsDevVersion(version string) bool {
	v, err := semver.NewVersion(strings.TrimPrefix(strings.TrimSpace(version), "v"))
	if err != nil {
		return false
	}
	return devPrerelease.MatchString(v.Prerelease())
}

func parseDevVersion(version string) (*semver.Version, error) {
	if !IsDevVersion(version) {
		return nil, fmt.Errorf("%q is not a dev version", version)
	}
	return semver.NewVersion(version)
}
