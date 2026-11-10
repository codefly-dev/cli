package composition

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/core/resources"
)

// SourceHead is what a source repository says about itself right now: the tip
// of its default branch and every tag it has published. It is the "is this pin
// current?" half of the question; the pin is the other half.
type SourceHead struct {
	// Branch is the default branch's short name ("main").
	Branch string
	// Tip is the commit that branch points at.
	Tip string
	// Tags are the published tag names, as `git ls-remote --tags` prints them
	// (annotated tags' peeled entries dropped).
	Tags []string
}

// ReadSourceHead asks source's remote for its default branch and tags in one
// round trip. An error means the remote could not be read, never "no such
// branch": the caller must treat it as "currency not verified".
func ReadSourceHead(ctx context.Context, source string) (*SourceHead, error) {
	url := PinnedSourceURL(source)
	var stderr bytes.Buffer
	command := gitCommand(ctx, "ls-remote", "--symref", "--", url, "HEAD", "refs/tags/*")
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = err.Error()
		}
		if first, _, found := strings.Cut(reason, "\n"); found {
			reason = first
		}
		return nil, fmt.Errorf("read %s: %s", source, reason)
	}
	return parseSourceHead(string(out))
}

// parseSourceHead reads `git ls-remote --symref <url> HEAD 'refs/tags/*'`: a
// "ref: refs/heads/main\tHEAD" line naming the default branch, the "<sha>\tHEAD"
// line giving its tip, then "<sha>\t<ref>" tag lines. Annotated tags also list a peeled
// "^{}" entry, which is dropped: only the name matters here.
func parseSourceHead(out string) (*SourceHead, error) {
	head := &SourceHead{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if branch, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			head.Branch, _, _ = strings.Cut(branch, "\t")
			continue
		}
		sha, ref, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		switch {
		case ref == "HEAD":
			head.Tip = sha
		case strings.HasPrefix(ref, "refs/tags/") && !strings.HasSuffix(ref, "^{}"):
			head.Tags = append(head.Tags, strings.TrimPrefix(ref, "refs/tags/"))
		}
	}
	if head.Branch == "" {
		return nil, fmt.Errorf("the remote names no default branch")
	}
	if head.Tip == "" {
		return nil, fmt.Errorf("the remote has no commit on its default branch %q", head.Branch)
	}
	return head, nil
}

// NewestRelease is the highest stable semver release among tags, with the
// plain "v" and the module-package prefix both read as the version they name.
// It is the bar a tag pin is held to; "" when no release is published.
func NewestRelease(tags []string) string {
	return strings.TrimPrefix(highestSemverTag(releaseVersions(tags), nil), "v")
}

// ReleasesNewerThan counts the releases published after version.
func ReleasesNewerThan(tags []string, version string) int {
	pinned, err := semver.NewVersion(strings.TrimPrefix(version, "v"))
	if err != nil {
		return 0
	}
	newer := 0
	for _, tag := range releaseVersions(tags) {
		if parsed, err := semver.NewVersion(strings.TrimPrefix(tag, "v")); err == nil && parsed.Prerelease() == "" && parsed.GreaterThan(pinned) {
			newer++
		}
	}
	return newer
}

func releaseVersions(tags []string) []string {
	versions := make([]string, 0, len(tags))
	for _, tag := range tags {
		versions = append(versions, strings.TrimPrefix(tag, modulePackageTagPrefix))
	}
	return versions
}

// Lag is how a commit stands against a source's default branch.
type Lag struct {
	// Behind counts the commits on the branch the commit does not have.
	Behind int
	// OnBranch is false when the commit is not in the branch's history: a PR
	// head not merged yet, a local commit, or a commit the history could not
	// find. Behind is then meaningless.
	OnBranch bool
}

// CommitLag measures commit against the default branch of source. History is
// read from a commit-only clone kept under the Codefly home (no trees, no
// blobs) and refreshed only when it lacks the tip or the commit, so a warm run
// is local. An error means history could not be had.
func CommitLag(ctx context.Context, source string, head *SourceHead, commit string) (Lag, error) {
	repo := filepath.Join(resources.CodeflyHomeDir(), "currency", historyDirName(source))
	if err := os.MkdirAll(repo, 0o755); err != nil {
		return Lag{}, err
	}
	git := func(args ...string) ([]byte, error) {
		return gitCommand(ctx, append([]string{"-C", repo}, args...)...).Output()
	}
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); err != nil {
		if _, err := git("init", "--bare", "--quiet"); err != nil {
			return Lag{}, fmt.Errorf("prepare the history of %s: %w", source, err)
		}
	}
	has := func(sha string) bool {
		_, err := git("cat-file", "-e", sha+"^{commit}")
		return err == nil
	}
	fetch := func(refspec string) error {
		_, err := git("fetch", "--quiet", "--no-tags", "--filter=tree:0", "--", PinnedSourceURL(source), refspec)
		return err
	}
	if !has(head.Tip) {
		if err := fetch("+refs/heads/" + head.Branch + ":refs/heads/" + head.Branch); err != nil {
			return Lag{}, fmt.Errorf("fetch the history of %s: %w", source, err)
		}
	}
	if !has(commit) {
		// A commit nothing on the branch reaches (an unmerged PR head) may still
		// be fetchable by name; failing to is an answer, not an error.
		_ = fetch(commit)
		if !has(commit) {
			return Lag{}, nil
		}
	}
	if _, err := git("merge-base", "--is-ancestor", commit, head.Tip); err != nil {
		return Lag{}, nil
	}
	out, err := git("rev-list", "--count", commit+".."+head.Tip)
	if err != nil {
		return Lag{}, err
	}
	behind, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return Lag{}, err
	}
	return Lag{Behind: behind, OnBranch: true}, nil
}

// historyDirName is the one directory name a source's history lives under: the
// URL it is fetched from with every separator flattened, so "owner/repo" and its
// full URL share a clone and a source can never name a path outside the cache.
func historyDirName(source string) string {
	return strings.NewReplacer("/", "_", ":", "_", "\\", "_", "..", "_").Replace(PinnedSourceURL(source))
}

// CheckoutLag reports how a local checkout stands against its source's default
// branch, the same measurement CommitLag makes for a pin.
func CheckoutLag(ctx context.Context, source string, head *SourceHead, dir string) (commit string, lag Lag, err error) {
	description, ok := DescribeCheckout(ctx, dir)
	if !ok || description.Commit == "" {
		return "", Lag{}, fmt.Errorf("%s is not a git checkout", dir)
	}
	lag, err = CommitLag(ctx, source, head, description.Commit)
	return description.Commit, lag, err
}
