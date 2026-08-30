package agents

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/publish"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/gh"
	"github.com/codefly-dev/core/resources"
	"github.com/google/go-github/v89/github"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// ReleaseCmd wraps the per-repo release cascade a fleet re-tag runs by hand —
// bump the manifest, open a PR, tag on merge, verify the asset shipped — into
// one verb, composing the checks the CLI already owns (`agent deps --pin`,
// `agent ci`, and the release-asset inventory behind `agent verify-platform`).
//
// The tag is created only AFTER a human merges the PR (branch protection is the
// policy; this command never merges). It is resumable: re-running after the
// merge picks the release up from wherever it left off — an open PR is waited
// on, a merged-but-untagged PR is tagged, an existing tag is verified.
var ReleaseCmd = &cobra.Command{
	Use:   "release",
	Short: "Bump, PR, tag-on-merge, and verify a service agent's release",
	Long: `release turns the manual per-repo release cascade into one verb:

  1. gate locally BEFORE tagging: pin core (--pin) then run agent CI
  2. bump the manifest to the next version above the authoritative remote tag
  3. open a PR from a release branch — a human merges it, per branch policy
  4. tag the merge commit, then verify the release published a downloadable
     asset — failing loudly if the tag shipped no artifact

It is safe to re-run. With no open work it verifies the current tag; with an
open PR it waits for the merge; with a merged PR it tags and verifies. Pass
--no-wait to open the PR and stop (tag + verify on a later re-run).

Requires the gh CLI to be authenticated (or GITHUB_TOKEN / GH_TOKEN set).

Examples:
  codefly agent release                     # patch bump, wait for merge, verify
  codefly agent release --pin latest        # pin core, then release
  codefly agent release --bump minor
  codefly agent release --no-wait           # open the PR only`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		o := releaseOptions{}
		o.dir, _ = cmd.Flags().GetString("dir")
		o.pin, _ = cmd.Flags().GetString("pin")
		o.bump, _ = cmd.Flags().GetString("bump")
		o.platform, _ = cmd.Flags().GetString("platform")
		o.noWait, _ = cmd.Flags().GetBool("no-wait")
		return runAgentRelease(ctx, o)
	},
}

func init() {
	ReleaseCmd.Flags().String("dir", "", "Agent source directory (default: current directory)")
	ReleaseCmd.Flags().String("pin", "", "Pin core to this published version before the CI gate (e.g. latest, v0.3.11)")
	ReleaseCmd.Flags().String("bump", "patch", "Version bump from the latest tag: patch | minor | major")
	ReleaseCmd.Flags().String("platform", "", "Additional os_arch the release must ship (e.g. linux_arm64); linux_amd64 is always required")
	ReleaseCmd.Flags().Bool("no-wait", false, "Open the PR and stop; re-run after merge to tag and verify")
}

type releaseOptions struct {
	dir      string
	pin      string
	bump     string
	platform string
	noWait   bool
}

// Poll cadence for the two waits (merge, then asset publish). Package-level so
// tests can shrink them; the defaults suit a human-merged PR and goreleaser CI.
var (
	mergePollInterval = 10 * time.Second
	mergePollTimeout  = 30 * time.Minute
	assetPollInterval = 15 * time.Second
	assetPollTimeout  = 20 * time.Minute
)

// runReleaseGate runs the pre-tag local gate (pin core, then agent CI). A seam
// so tests can drive the release flow without a real, minutes-long CI build.
var runReleaseGate = runReleaseGateExec

func runAgentRelease(ctx context.Context, o releaseOptions) error {
	if _, err := bumpFrom(&semver.Version{}, o.bump); err != nil {
		return err
	}
	if gh.Token() == "" {
		return fmt.Errorf("a GitHub token is required to open the release PR and tag; set GITHUB_TOKEN or GH_TOKEN, or authenticate the gh CLI (gh auth login)")
	}

	target, manifest, err := resolveReleaseTarget(ctx, o.dir)
	if err != nil {
		return err
	}

	newVer, err := target.nextReleaseVersion(ctx, manifest, o.bump)
	if err != nil {
		return err
	}
	newTag := "v" + newVer.String()
	extraPlatform := normalizePlatform(o.platform)

	// The tag already exists: nothing to release, only to (re)confirm it
	// shipped an asset. Makes a re-run after a completed release a pure verify.
	exists, err := target.tagExistsOnOrigin(ctx, newTag)
	if err != nil {
		return err
	}
	if exists {
		cli.Info("tag %s already exists on origin; verifying its release asset", newTag)
		return target.verifyPublishedAsset(ctx, newVer.String(), extraPlatform)
	}

	branch := "release/" + newTag
	pr, err := target.findReleasePR(ctx, branch)
	if err != nil {
		return err
	}
	if pr == nil {
		cli.Info("releasing %s/%s %s (from %s)", target.agent.Publisher, target.agent.Name, newTag, manifest.Version)
		pr, err = target.openReleasePR(ctx, o, manifest, newVer, branch)
		if err != nil {
			return err
		}
	} else {
		cli.Info("resuming release %s from existing PR %s", newTag, pr.GetHTMLURL())
	}

	if o.noWait && !pr.GetMerged() {
		cli.Info("PR %s opened; merge it then re-run `codefly agent release` to tag and verify", pr.GetHTMLURL())
		return nil
	}

	mergeSHA := pr.GetMergeCommitSHA()
	if !pr.GetMerged() || mergeSHA == "" {
		cli.Info("waiting for %s to be merged (human-merged per branch policy)...", pr.GetHTMLURL())
		mergeSHA, err = target.waitForMerge(ctx, pr.GetNumber())
		if err != nil {
			return err
		}
	}

	cli.Info("PR merged at %s; tagging %s", mergeSHA[:min(len(mergeSHA), 12)], newTag)
	if err := target.createTagRef(ctx, newTag, mergeSHA); err != nil {
		return err
	}

	return target.verifyPublishedAsset(ctx, newVer.String(), extraPlatform)
}

// releaseTarget is the resolved subject of a release: where its git repo and
// manifest live, and the GitHub coordinates its assets publish to.
type releaseTarget struct {
	dir     string // agent directory (holds agent.codefly.yaml)
	workDir string // git repo root
	owner   string // GitHub owner
	repo    string // GitHub repository
	agent   *resources.Agent
}

func resolveReleaseTarget(ctx context.Context, dir string) (*releaseTarget, *publish.Manifest, error) {
	if strings.TrimSpace(dir) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, nil, fmt.Errorf("get agent directory: %w", err)
		}
		dir = cwd
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve agent directory: %w", err)
	}

	manifest, err := publish.Detect(dir)
	if err != nil {
		return nil, nil, err
	}
	if manifest.Mode != publish.ModeAgent {
		return nil, nil, fmt.Errorf("release is for agent repositories (agent.codefly.yaml); detected %s", manifest.Mode)
	}

	identity, err := readReleaseIdentity(filepath.Join(dir, "agent.codefly.yaml"))
	if err != nil {
		return nil, nil, err
	}
	if identity.Kind != serviceAgentKind {
		return nil, nil, fmt.Errorf("release supports service agents (%s); this agent is %q", serviceAgentKind, identity.Kind)
	}

	workDir, err := gitRun(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, nil, fmt.Errorf("find git root for %s: %w", dir, err)
	}
	agent := &resources.Agent{
		Kind:      resources.ServiceAgent,
		Publisher: identity.Publisher,
		Name:      identity.Name,
	}
	return &releaseTarget{
		dir:     dir,
		workDir: strings.TrimSpace(workDir),
		owner:   gh.Owner(identity.Publisher),
		repo:    "service-" + identity.Name,
		agent:   agent,
	}, manifest, nil
}

type releaseIdentity struct {
	Publisher string `yaml:"publisher"`
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
}

func readReleaseIdentity(path string) (releaseIdentity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return releaseIdentity{}, fmt.Errorf("read agent.codefly.yaml: %w", err)
	}
	var id releaseIdentity
	if err := yaml.Unmarshal(raw, &id); err != nil {
		return releaseIdentity{}, fmt.Errorf("parse agent.codefly.yaml: %w", err)
	}
	if id.Publisher == "" || id.Kind == "" || id.Name == "" {
		return releaseIdentity{}, fmt.Errorf("agent.codefly.yaml must have publisher, kind, and name")
	}
	return id, nil
}

// nextReleaseVersion bumps from the AUTHORITATIVE remote tag, not the manifest.
// The manifest version lags whenever a tag is cut without a matching commit, so
// bumping it alone collides with an existing tag; bumping from max(manifest,
// latest origin tag) yields the next free version.
func (t *releaseTarget) nextReleaseVersion(ctx context.Context, m *publish.Manifest, bump string) (*semver.Version, error) {
	base := m.Version
	latest, err := t.latestRemoteTag(ctx)
	if err != nil {
		return nil, err
	}
	if latest != nil && latest.GreaterThan(base) {
		base = latest
	}
	next, err := bumpFrom(base, bump)
	if err != nil {
		return nil, err
	}
	return &next, nil
}

func bumpFrom(base *semver.Version, bump string) (semver.Version, error) {
	switch bump {
	case "", "patch":
		return base.IncPatch(), nil
	case "minor":
		return base.IncMinor(), nil
	case "major":
		return base.IncMajor(), nil
	default:
		return semver.Version{}, fmt.Errorf("invalid bump type %q (use patch | minor | major)", bump)
	}
}

// latestRemoteTag returns the highest v* semver tag on origin, or nil when the
// repo has no version tags yet. Annotated-tag deref lines (refs/tags/vX^{}) and
// non-version tags fail to parse and are skipped.
func (t *releaseTarget) latestRemoteTag(ctx context.Context) (*semver.Version, error) {
	out, err := t.git(ctx, "ls-remote", "--tags", "origin")
	if err != nil {
		return nil, fmt.Errorf("list origin tags: %w", err)
	}
	var best *semver.Version
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		ref := strings.TrimPrefix(fields[1], "refs/tags/")
		if !strings.HasPrefix(ref, "v") {
			continue
		}
		v, err := semver.NewVersion(strings.TrimPrefix(ref, "v"))
		if err != nil {
			continue
		}
		if best == nil || v.GreaterThan(best) {
			best = v
		}
	}
	return best, nil
}

func (t *releaseTarget) tagExistsOnOrigin(ctx context.Context, tag string) (bool, error) {
	out, err := t.git(ctx, "ls-remote", "--tags", "origin", "refs/tags/"+tag)
	if err != nil {
		return false, fmt.Errorf("check origin for tag %s: %w", tag, err)
	}
	return strings.TrimSpace(out) != "", nil
}

// openReleasePR runs the local gate, bumps the manifest on a release branch,
// commits (pin + version bump together), pushes, and opens the PR. On any
// failure before the PR is open it restores the working tree to main so the
// operator's checkout is left exactly as it was found.
func (t *releaseTarget) openReleasePR(ctx context.Context, o releaseOptions, m *publish.Manifest, newVer *semver.Version, branch string) (_ *github.PullRequest, err error) {
	if perr := t.assertReleasable(ctx); perr != nil {
		return nil, perr
	}
	if _, cerr := t.git(ctx, "checkout", "-b", branch); cerr != nil {
		return nil, fmt.Errorf("create release branch %s: %w", branch, cerr)
	}
	defer func() {
		if err != nil {
			t.abortBranch(ctx, branch)
		}
	}()

	from := m.Version.String()
	if err = runReleaseGate(ctx, t.dir, o.pin); err != nil {
		return nil, err
	}
	if err = m.WriteVersion(newVer); err != nil {
		return nil, fmt.Errorf("write version: %w", err)
	}

	newTag := "v" + newVer.String()
	if err = t.commitAndPush(ctx, branch, newTag); err != nil {
		return nil, err
	}

	client, err := gh.NewClient()
	if err != nil {
		return nil, err
	}
	pr, _, err := client.PullRequests.Create(ctx, t.owner, t.repo, &github.NewPullRequest{
		Title: github.Ptr("release: " + newTag),
		Head:  github.Ptr(branch),
		Base:  github.Ptr("main"),
		Body:  github.Ptr(releasePRBody(o, from, newTag)),
	})
	if err != nil {
		return nil, fmt.Errorf("open release pull request: %w", err)
	}
	// Back to main so the operator's working tree is normal while the PR is
	// reviewed; the release commit lives on the branch and, once merged, on main.
	if _, cerr := t.git(ctx, "checkout", "-f", "main"); cerr != nil {
		return nil, fmt.Errorf("return to main after opening PR: %w", cerr)
	}
	cli.Info("opened %s", pr.GetHTMLURL())
	return pr, nil
}

func releasePRBody(o releaseOptions, from, tag string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Release %s (from %s).\n\n", tag, from)
	if o.pin != "" {
		fmt.Fprintf(&b, "- core pinned to `%s`\n", o.pin)
	}
	b.WriteString("- gated on `agent ci` before tagging\n")
	b.WriteString("- tag is cut on merge; the published asset is verified after\n")
	return b.String()
}

func (t *releaseTarget) findReleasePR(ctx context.Context, branch string) (*github.PullRequest, error) {
	client, err := gh.NewClient()
	if err != nil {
		return nil, err
	}
	prs, _, err := client.PullRequests.List(ctx, t.owner, t.repo, &github.PullRequestListOptions{
		State:       "all",
		Head:        t.owner + ":" + branch,
		Sort:        "created",
		Direction:   "desc",
		ListOptions: github.ListOptions{PerPage: 10},
	})
	if err != nil {
		return nil, fmt.Errorf("list release pull requests: %w", err)
	}
	if len(prs) == 0 {
		return nil, nil
	}
	pr := prs[0]
	if pr.GetState() == "closed" && !pr.GetMerged() {
		return nil, fmt.Errorf("release PR %s was closed without merging; delete branch %s to start over", pr.GetHTMLURL(), branch)
	}
	return pr, nil
}

// waitForMerge polls the PR until a human merges it, returning the merge commit
// SHA to tag. A closed-unmerged PR or the poll timeout fails loudly.
func (t *releaseTarget) waitForMerge(ctx context.Context, number int) (string, error) {
	client, err := gh.NewClient()
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(mergePollTimeout)
	for {
		pr, _, err := client.PullRequests.Get(ctx, t.owner, t.repo, number)
		if err == nil {
			if pr.GetMerged() {
				sha := pr.GetMergeCommitSHA()
				if sha == "" {
					return "", fmt.Errorf("PR #%d is merged but reports no merge commit SHA", number)
				}
				return sha, nil
			}
			if pr.GetState() == "closed" {
				return "", fmt.Errorf("PR #%d was closed without merging", number)
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for PR #%d to merge; merge it then re-run `codefly agent release`", number)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(mergePollInterval):
		}
	}
}

// createTagRef creates refs/tags/<tag> at the merge commit via the API, the
// same lightweight tag the hand cascade cut with `gh api .../git/refs`. Pushing
// the tag is what triggers the repo's release CI. Idempotent: an already-present
// tag (a re-run after the ref was created) is treated as success.
func (t *releaseTarget) createTagRef(ctx context.Context, tag, sha string) error {
	client, err := gh.NewClient()
	if err != nil {
		return err
	}
	_, _, err = client.Git.CreateRef(ctx, t.owner, t.repo, github.CreateRef{
		Ref: "refs/tags/" + tag,
		SHA: sha,
	})
	if err != nil {
		var ge *github.ErrorResponse
		if errors.As(err, &ge) && ge.Response != nil &&
			ge.Response.StatusCode == http.StatusUnprocessableEntity &&
			strings.Contains(strings.ToLower(ge.Message), "already exists") {
			return nil
		}
		return fmt.Errorf("create tag %s at %s: %w", tag, sha, err)
	}
	return nil
}

// verifyPublishedAsset polls the agent's releases until the new version ships a
// downloadable asset for every required platform (always linux_amd64, plus an
// optional extra target), or the poll times out. A tag with no artifact is the
// exact failure this makes loud rather than leaving for a runtime 404.
func (t *releaseTarget) verifyPublishedAsset(ctx context.Context, version, extraPlatform string) error {
	required := []string{ciPlatform}
	if extraPlatform != "" && extraPlatform != ciPlatform {
		required = append(required, extraPlatform)
	}

	deadline := time.Now().Add(assetPollTimeout)
	var shipped []string
	var lastErr error
	for {
		releases, err := fetchReleases(ctx, t.agent)
		if err != nil {
			lastErr = err
		} else if platforms, ok := releasePlatforms(releases, version); ok {
			shipped = platforms
			if missing := missingPlatforms(required, platforms); len(missing) == 0 {
				cli.Info("release v%s ships [%s]", version, strings.Join(platforms, " "))
				return nil
			}
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(assetPollInterval):
		}
	}
	if lastErr != nil && shipped == nil {
		return fmt.Errorf("could not confirm release v%s published an asset: %w", version, lastErr)
	}
	return fmt.Errorf("release v%s did not publish required asset(s) [%s] (shipped: [%s]); the tag has no usable artifact",
		version, strings.Join(missingPlatforms(required, shipped), " "), strings.Join(shipped, " "))
}

func releasePlatforms(releases []releaseInfo, version string) ([]string, bool) {
	for _, release := range releases {
		if release.version == version {
			return release.platforms, true
		}
	}
	return nil, false
}

func missingPlatforms(required, shipped []string) []string {
	var missing []string
	for _, want := range required {
		if !slices.Contains(shipped, want) {
			missing = append(missing, want)
		}
	}
	return missing
}

// --- git + gate helpers --------------------------------------------------

func (t *releaseTarget) assertReleasable(ctx context.Context) error {
	out, err := t.git(ctx, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("working tree has uncommitted changes — commit or stash first:\n%s", out)
	}
	branch, err := t.git(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("git rev-parse: %w", err)
	}
	if strings.TrimSpace(branch) != "main" {
		return fmt.Errorf("not on main (on %q); release opens its PR from main", strings.TrimSpace(branch))
	}
	return nil
}

func (t *releaseTarget) commitAndPush(ctx context.Context, branch, tag string) error {
	if _, err := t.git(ctx, "add", "-A"); err != nil {
		return fmt.Errorf("stage release changes: %w", err)
	}
	if _, err := t.git(ctx, "commit", "-m", "release: "+tag); err != nil {
		return fmt.Errorf("commit release: %w", err)
	}
	if _, err := t.git(ctx, "push", "-u", "origin", branch); err != nil {
		return fmt.Errorf("push release branch: %w", err)
	}
	return nil
}

// abortBranch restores the operator's checkout to main after a failed release,
// discarding the branch's uncommitted gate/bump changes (all reproducible).
func (t *releaseTarget) abortBranch(ctx context.Context, branch string) {
	_, _ = t.git(ctx, "checkout", "-f", "main")
	_, _ = t.git(ctx, "branch", "-D", branch)
}

func (t *releaseTarget) git(ctx context.Context, args ...string) (string, error) {
	return gitRun(ctx, t.workDir, args...)
}

// gitRun runs a git subcommand in dir. The git CLI (not go-git) is used so the
// repo's signing config (commit.gpgsign etc.) applies to the release commit and
// tag automatically. A seam for tests.
var gitRun = func(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func runReleaseGateExec(ctx context.Context, dir, pin string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve codefly executable: %w", err)
	}
	if pin != "" {
		if err = runSelf(ctx, self, dir, "agent", "deps", "--pin", pin, "--dir", dir); err != nil {
			return fmt.Errorf("pin core to %s: %w", pin, err)
		}
	}
	// CI writes its report/artifacts to a temp dir so the release branch stays
	// clean except for the pin and version bump we intend to commit.
	output, err := os.MkdirTemp("", "codefly-agent-release-ci-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(output)
	if err = runSelf(ctx, self, dir, "agent", "ci", "--dir", dir, "--output", output); err != nil {
		return fmt.Errorf("agent ci gate failed — not tagging: %w", err)
	}
	return nil
}

func runSelf(ctx context.Context, self, dir string, args ...string) error {
	//nolint:gosec // self is os.Executable() and args are internally constructed.
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
