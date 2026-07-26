package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Masterminds/semver"
)

// Engine runs the actual release flow. Holds the Manifest plus the
// caller's choices (bump type, dry-run, working dir). Created once
// per `codefly publish` invocation.
type Engine struct {
	Manifest            *Manifest
	AdditionalManifests []*Manifest
	BumpType            string // "patch" | "minor" | "major"
	DryRun              bool
	WorkDir             string // git operations run from here; defaults to manifest's dir parent
	SignTag             bool

	// BeforeCommit, when set, runs after the bumped version is written to
	// the manifest (the working tree now carries the new version) but
	// before any git mutation. Returning an error restores the manifest
	// and aborts with nothing committed, tagged, or pushed. Agent mode
	// uses it to run release-grade CI and stage loader release assets
	// built from the exact version being tagged.
	BeforeCommit func(ctx context.Context, newTag string) error

	// AfterPush, when set, runs after the tag is pushed to origin. Agent
	// mode uses it to create the GitHub release, upload the loader
	// assets, and verify them. An error is returned with the tag already
	// live — the caller reports it rather than pretending it didn't ship.
	AfterPush func(ctx context.Context, newTag string) error
}

// Release executes the full publish flow — pre-flight gates, bump,
// commit, tag, push. Returns the new tag on success.
//
// Order matters: pre-flight FIRST, modifications SECOND. Every
// failure surface should be aborted with the working tree untouched
// where possible.
func (e *Engine) Release(ctx context.Context) (string, error) {
	if err := e.preflight(ctx); err != nil {
		return "", err
	}

	// Reconcile the bump base against what's ACTUALLY been released. The
	// manifest's version drifts behind the git tags (tags get cut by CI /
	// older tooling without bumping info.codefly.yaml), so bumping the
	// manifest alone collides with an existing tag. Bump from
	// max(manifest, latest tag) instead — the version we've truly shipped.
	if latest := e.latestTaggedVersion(ctx); latest != nil && latest.GreaterThan(e.Manifest.Version) {
		fmt.Printf("==> manifest %s is behind latest tag v%s; bumping from v%s\n",
			e.Manifest.Version, latest, latest)
		e.Manifest.Version = latest
	}

	manifests := e.manifests()
	for _, manifest := range manifests[1:] {
		manifest.Version = e.Manifest.Version
	}
	newVer, err := e.Manifest.Bump(e.BumpType)
	if err != nil {
		return "", fmt.Errorf("bump version: %w", err)
	}
	newTag := "v" + newVer.String()

	// Pre-flight tag-existence check uses the bumped version (not the
	// current). Done after bump so we can name the actual tag we'd
	// create in the error message.
	if err := e.assertTagDoesNotExist(ctx, newTag); err != nil {
		return "", err
	}

	if e.DryRun {
		fmt.Printf("[dry-run] would bump %s → %s and tag %s\n",
			e.Manifest.Version, newVer, newTag)
		return newTag, nil
	}

	for _, manifest := range manifests {
		if err := manifest.WriteVersion(newVer); err != nil {
			if restoreErr := e.restoreManifest(ctx); restoreErr != nil {
				return "", errors.Join(fmt.Errorf("write version: %w", err), restoreErr)
			}
			return "", fmt.Errorf("write version: %w", err)
		}
	}

	// Gate on BeforeCommit while the tree is still pristine except for the
	// version bump. A failure here (e.g. release CI failed, or a required
	// platform is missing) must leave the repo exactly as it was found.
	if e.BeforeCommit != nil {
		if err := e.BeforeCommit(ctx, newTag); err != nil {
			if restoreErr := e.restoreManifest(ctx); restoreErr != nil {
				return "", errors.Join(err, restoreErr)
			}
			return "", err
		}
	}

	if err := e.gitCommit(ctx, newTag); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	if err := e.gitTag(ctx, newTag); err != nil {
		return "", fmt.Errorf("tag: %w", err)
	}
	if err := e.gitPush(ctx, newTag); err != nil {
		return "", fmt.Errorf("push: %w", err)
	}

	if e.AfterPush != nil {
		if err := e.AfterPush(ctx, newTag); err != nil {
			return newTag, err
		}
	}
	return newTag, nil
}

// restoreManifest reverts the working-tree version bump after an aborted
// release. Pre-flight guaranteed a clean tree, so checking out the
// manifest restores the previously committed version verbatim.
func (e *Engine) restoreManifest(ctx context.Context) error {
	args := []string{"checkout", "--"}
	for _, manifest := range e.manifests() {
		args = append(args, manifest.Path)
	}
	if _, err := e.git(ctx, args...); err != nil {
		return fmt.Errorf("restore manifest after aborted release: %w", err)
	}
	return nil
}

func (e *Engine) manifests() []*Manifest {
	return append([]*Manifest{e.Manifest}, e.AdditionalManifests...)
}

// --- Pre-flight ----------------------------------------------------

func (e *Engine) preflight(ctx context.Context) error {
	if err := e.assertCleanTree(ctx); err != nil {
		return err
	}
	if err := e.assertOnMain(ctx); err != nil {
		return err
	}
	if err := e.assertSyncedWithOrigin(ctx); err != nil {
		return err
	}
	return nil
}

func (e *Engine) assertCleanTree(ctx context.Context) error {
	out, err := e.git(ctx, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("working tree has uncommitted changes — commit or stash first:\n%s", out)
	}
	return nil
}

func (e *Engine) assertOnMain(ctx context.Context) error {
	out, err := e.git(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("git rev-parse: %w", err)
	}
	branch := strings.TrimSpace(out)
	if branch != "main" {
		return fmt.Errorf("not on main (on %q); releases tag from main only", branch)
	}
	return nil
}

func (e *Engine) assertSyncedWithOrigin(ctx context.Context) error {
	if _, err := e.git(ctx, "fetch", "origin", "main", "--quiet"); err != nil {
		return fmt.Errorf("fetch origin/main: %w", err)
	}
	// Refuse only when local is BEHIND origin (a release commit couldn't
	// fast-forward) or the histories diverged. Being purely AHEAD is fine:
	// the release commit + tag push fast-forwards origin. This lets you
	// commit local changes (e.g. a toolchain bump) and publish in one step
	// without a separate manual push first.
	counts, err := e.git(ctx, "rev-list", "--left-right", "--count", "origin/main...HEAD")
	if err != nil {
		return fmt.Errorf("compare with origin/main: %w", err)
	}
	fields := strings.Fields(counts)
	if len(fields) != 2 {
		return fmt.Errorf("unexpected rev-list output %q", strings.TrimSpace(counts))
	}
	behind := fields[0]
	if behind != "0" {
		return fmt.Errorf("local main is behind origin/main by %s commit(s); pull first — refusing to overwrite", behind)
	}
	return nil
}

func (e *Engine) assertTagDoesNotExist(ctx context.Context, tag string) error {
	if _, err := e.git(ctx, "rev-parse", tag); err == nil {
		return fmt.Errorf("tag %s already exists locally — delete it or pick a different bump", tag)
	}
	out, err := e.git(ctx, "ls-remote", "--tags", "origin", "refs/tags/"+tag)
	if err != nil {
		return fmt.Errorf("ls-remote check: %w", err)
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("tag %s already exists on origin — pick a different bump", tag)
	}
	return nil
}

// latestTaggedVersion returns the highest semver among this repo's
// `v*` tags, considering BOTH local tags and origin's (the manifest is
// not authoritative — tags are). Returns nil when there are no parseable
// version tags yet (first release).
func (e *Engine) latestTaggedVersion(ctx context.Context) *semver.Version {
	var best *semver.Version
	consider := func(ref string) {
		ref = strings.TrimSpace(ref)
		ref = strings.TrimPrefix(ref, "refs/tags/")
		if !strings.HasPrefix(ref, "v") {
			return
		}
		// Annotated-tag deref lines (refs/tags/vX^{}) fail to parse and
		// are skipped, which is what we want.
		v, err := semver.NewVersion(strings.TrimPrefix(ref, "v"))
		if err != nil {
			return
		}
		if best == nil || v.GreaterThan(best) {
			best = v
		}
	}
	if out, err := e.git(ctx, "tag", "-l", "v*"); err == nil {
		for l := range strings.SplitSeq(out, "\n") {
			consider(l)
		}
	}
	if out, err := e.git(ctx, "ls-remote", "--tags", "origin"); err == nil {
		for l := range strings.SplitSeq(out, "\n") {
			fields := strings.Fields(l)
			if len(fields) == 2 {
				consider(fields[1])
			}
		}
	}
	return best
}

// --- Mutations -----------------------------------------------------

// gitCommit stages the manifest and creates a release commit. The
// commit message is `release: <tag>` — short, indexable.
func (e *Engine) gitCommit(ctx context.Context, tag string) error {
	args := []string{"add"}
	for _, manifest := range e.manifests() {
		args = append(args, manifest.Path)
	}
	if _, err := e.git(ctx, args...); err != nil {
		return err
	}
	if _, err := e.git(ctx, "commit", "-m", "release: "+tag); err != nil {
		return err
	}
	return nil
}

// gitTag creates an ANNOTATED tag. `git tag -a` (annotated) over `git
// tag` (lightweight) is the codefly convention — annotated tags carry
// a tagger identity + message + are GPG-signable when the repo's
// signing config is on.
func (e *Engine) gitTag(ctx context.Context, tag string) error {
	mode := "-a"
	if e.SignTag {
		mode = "-s"
	}
	_, err := e.git(ctx, "tag", mode, tag, "-m", "Version "+strings.TrimPrefix(tag, "v"))
	return err
}

// gitPush pushes BOTH the commit and the tag, in two separate
// invocations. Two pushes (not `git push --follow-tags`) so a tag
// failure surfaces independently of a commit failure.
//
// Crucially: NO --force on either. The pre-flight gates ensure the
// state is clean enough that a non-force push always succeeds.
func (e *Engine) gitPush(ctx context.Context, tag string) error {
	if _, err := e.git(ctx, "push", "origin", "main"); err != nil {
		return fmt.Errorf("push main: %w", err)
	}
	if _, err := e.git(ctx, "push", "origin", tag); err != nil {
		return fmt.Errorf("push tag: %w", err)
	}
	return nil
}

// --- Re-tag mode ---------------------------------------------------

// ReTag re-cuts an existing tag at the current HEAD. Used when CI
// needs a fresh tag-push trigger (e.g. release artifacts didn't
// build on the original push) without bumping the version.
//
// This is the ONE place we accept a force-push — but only on the
// TAG, never on main. Main is left alone; only the tag's destination
// changes.
//
// Pre-flight subset: clean tree, on main, in sync with origin. We
// don't assert "tag doesn't exist" because the whole point is the
// tag DOES exist and needs replacing.
func (e *Engine) ReTag(ctx context.Context) (string, error) {
	if err := e.preflight(ctx); err != nil {
		return "", err
	}
	tag := e.Manifest.Tag()

	// Confirm the tag DOES exist, somewhere — local or remote. If it
	// doesn't, the operator probably meant to use the regular
	// publish path; surface the misuse.
	localExists := e.tagExistsLocally(ctx, tag)
	remoteExists, err := e.tagExistsRemotely(ctx, tag)
	if err != nil {
		return "", err
	}
	if !localExists && !remoteExists {
		return "", fmt.Errorf("re-tag: %s does not exist locally OR on origin; use `codefly publish` (without --re-tag) to create it for the first time", tag)
	}

	if e.DryRun {
		fmt.Printf("[dry-run] would re-tag %s at HEAD (delete + recreate + force-push the tag only)\n", tag)
		return tag, nil
	}

	// Re-tag is the recovery path for an agent whose release assets never
	// uploaded (that is its documented purpose). Rebuild them from the
	// current version before moving the tag; no manifest is written, so a
	// failure here needs no revert.
	if e.BeforeCommit != nil {
		if err := e.BeforeCommit(ctx, tag); err != nil {
			return "", err
		}
	}

	// Delete locally first (idempotent).
	if localExists {
		if _, err := e.git(ctx, "tag", "-d", tag); err != nil {
			return "", fmt.Errorf("delete local tag: %w", err)
		}
	}
	// Recreate at HEAD.
	if err := e.gitTag(ctx, tag); err != nil {
		return "", fmt.Errorf("recreate tag: %w", err)
	}
	// Force-push the tag specifically. NOT main.
	if _, err := e.git(ctx, "push", "origin", tag, "--force"); err != nil {
		return "", fmt.Errorf("force-push tag: %w", err)
	}

	if e.AfterPush != nil {
		if err := e.AfterPush(ctx, tag); err != nil {
			return tag, err
		}
	}
	return tag, nil
}

func (e *Engine) tagExistsLocally(ctx context.Context, tag string) bool {
	_, err := e.git(ctx, "rev-parse", tag)
	return err == nil
}

func (e *Engine) tagExistsRemotely(ctx context.Context, tag string) (bool, error) {
	out, err := e.git(ctx, "ls-remote", "--tags", "origin", "refs/tags/"+tag)
	if err != nil {
		return false, fmt.Errorf("ls-remote: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

// --- Git invocation helper -----------------------------------------

// git runs a git subcommand in e.WorkDir and returns its stdout. We
// use the git CLI rather than go-git because the CLI honors the
// repo's signing config (commit.gpgsign etc.) automatically — go-git
// would require us to wire signing manually, and the codefly
// convention is signed commits everywhere.
func (e *Engine) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if e.WorkDir != "" {
		cmd.Dir = e.WorkDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		stderrTail := strings.TrimSpace(stderr.String())
		if stderrTail != "" {
			return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, stderrTail)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}
