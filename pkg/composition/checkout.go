package composition

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/Masterminds/semver"
)

// CheckoutDescription is what git says a working tree is: the nearest tag
// reachable from its HEAD, and how many commits it sits past that tag.
type CheckoutDescription struct {
	// Raw is `git describe --tags` verbatim ("v0.0.62", "v0.0.62-7-gbcd15ad"),
	// the form a diagnostic quotes back.
	Raw   string
	Tag   string
	Ahead int
}

// describeAhead splits the "-<n>-g<sha>" suffix `git describe` appends when
// HEAD is not the tag itself. A tag may contain dashes, so the suffix is
// anchored at the end rather than cut at the first one.
var describeAhead = regexp.MustCompile(`^(.+)-([0-9]+)-g[0-9a-f]+$`)

// gitRead runs a read-only git query in dir. Optional locks are disabled so
// that inspecting a checkout cannot write to its repository.
func gitRead(ctx context.Context, dir string, args ...string) (string, bool) {
	//nolint:gosec // G204: every arg is a literal from this file; dir is the checkout being inspected.
	command := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	command.Env = append(command.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := command.Output()
	if err != nil {
		return "", false
	}
	text := strings.TrimSpace(string(out))
	return text, text != ""
}

// CheckoutRoot is the root of the git working tree dir belongs to, or false
// when dir is not inside one.
func CheckoutRoot(ctx context.Context, dir string) (string, bool) {
	return gitRead(ctx, dir, "rev-parse", "--show-toplevel")
}

// DescribeCheckout reports which version dir's checkout holds. It answers
// false whenever git cannot say — dir is not a checkout, git is missing, or no
// tag is reachable from HEAD, which is the normal state of the shallow
// submodule clones CI produces. A checkout git cannot describe is not evidence
// of drift, so callers stay silent on it rather than reporting a version they
// did not establish.
func DescribeCheckout(ctx context.Context, dir string) (*CheckoutDescription, bool) {
	raw, ok := gitRead(ctx, dir, "describe", "--tags")
	if !ok {
		return nil, false
	}
	description := &CheckoutDescription{Raw: raw, Tag: raw}
	if match := describeAhead.FindStringSubmatch(raw); match != nil {
		description.Tag = match[1]
		ahead, err := strconv.Atoi(match[2])
		if err != nil {
			return nil, false
		}
		description.Ahead = ahead
	}
	return description, true
}

// SatisfiesVersion reports whether the checkout is the version a composition
// pins: sitting exactly on a tag naming that version, with no commits past it.
// A commit past the tag is a different tree than the pin names, whatever the
// tag was.
func (description *CheckoutDescription) SatisfiesVersion(version string) bool {
	if description.Ahead > 0 {
		return false
	}
	requested := requestedVersion(version)
	if requested == latestVersion {
		return true
	}
	tag := checkoutTagVersion(description.Tag)
	if requestedVersion(tag) == requested {
		return true
	}
	constraint, err := semver.NewConstraint(requested)
	if err != nil {
		return false
	}
	parsed, err := semver.NewVersion(tag)
	if err != nil {
		return false
	}
	return constraint.Check(parsed)
}

// checkoutTagVersion strips the tag conventions a module package is published
// under (see modulePackageTagPrefix) down to the bare version the composition
// pins.
func checkoutTagVersion(tag string) string {
	return strings.TrimPrefix(strings.TrimPrefix(tag, modulePackageTagPrefix), "v")
}
