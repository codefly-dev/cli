package prerelease

import (
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
	"golang.org/x/mod/module"
)

// Kind is how a prerelease is spelled. Both are the same defect — a version
// naming a build that was never released — but they read differently enough in a
// failure report to be worth distinguishing.
type Kind string

const (
	// KindPrereleaseTag is a semver prerelease component: 0.1.48-dev.e87db5e08865,
	// 0.2.0-rc.1, 1.0.0-alpha.
	KindPrereleaseTag Kind = "prerelease-tag"
	// KindPseudoVersion is a Go pseudo-version, which encodes a commit that no tag
	// names: v0.0.0-20260930123456-abcdef123456, v0.5.11-0.20260927230309-0c1b5db823d5.
	KindPseudoVersion Kind = "pseudo-version"
)

// Classify reports whether a version string is a prerelease, and how it is
// spelled.
//
// Detection is the *shape*, never the literal "dev": any semver prerelease
// component counts, and a Go pseudo-version is recognised as one because it is
// one. Anything that is not an exact semantic version is not a prerelease and is
// left alone — "latest" is the agent sentinel, "^0.0.1" and "~1.2" are the range
// constraints a library dependency may carry, and neither names a build.
// Semver build metadata ("1.2.3+build.5") is not a prerelease either.
func Classify(version string) (Kind, bool) {
	version = strings.TrimSpace(version)
	if version == "" {
		return "", false
	}
	parsed, err := semver.NewVersion(version)
	if err != nil {
		return "", false
	}
	if parsed.Prerelease() == "" {
		return "", false
	}
	// module.IsPseudoVersion wants the canonical v-prefixed spelling; a codefly
	// pin omits the v, a go.mod require carries it.
	candidate := version
	if !strings.HasPrefix(candidate, "v") {
		candidate = "v" + candidate
	}
	if module.IsPseudoVersion(candidate) {
		return KindPseudoVersion, true
	}
	return KindPrereleaseTag, true
}

// issueReference is what makes an agent-overrides comment a label rather than a
// remark: it names the issue the override stands in for, so the reader of the
// block knows what to watch for before it can be dropped. Matches both
// "codefly-dev/service-go#117" and a bare "#117".
var issueReference = regexp.MustCompile(`(?:[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*)?#\d+`)

// Labelled reports whether comment text labels an override: a non-empty comment
// carrying an issue reference. The condition for removal is prose a gate cannot
// check; the issue reference is the half that can be, and is the half that tells
// the next reader where to look.
func Labelled(comment string) bool {
	return issueReference.MatchString(comment)
}
