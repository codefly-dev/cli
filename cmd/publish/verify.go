package publish

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/gh"
	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/agents/manager"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/google/go-github/v89/github"
	"github.com/spf13/cobra"
)

// verifyCmd implements `codefly publish verify` — the same verification
// `codefly publish` runs after it pushes a tag, available on its own so a
// release can be established long after the publish that made it, from a
// machine that never built it.
//
// Everything here reads the published artefact. Nothing reads the working tree
// that produced it, and nothing reads the local checkout's main: a worktree's
// main can be hundreds of commits behind, and a check that read it would
// answer about that stale copy rather than about the release.
var verifyCmd = &cobra.Command{
	Use:   "verify [version]",
	Short: "Verify a published release exists, downloads, and runs",
	Long: `verify establishes that a release is real, by reading what was
published rather than what was built:

  - the version main declares is named by a tag, and that tag's commit is one
    main carries
  - the release is not a draft and carries a loader archive for every required
    platform (darwin/arm64, linux/amd64)
  - every archive matches the digest its producer published, the digest GitHub
    recorded, and the bytes GitHub serves
  - the archive is reachable with the credential the install path itself uses
    (GITHUB_TOKEN/GH_TOKEN only — never a gh CLI login)
  - the archive installs, the agent loads, and it advertises a contract this
    CLI can speak

With no argument it verifies the version the manifest on main declares. A
version argument verifies that release instead, so an older one can be
established after the fact.

A module or provider agent ships only its signed tag, so the tag check is the
whole verification for one; there are no assets to read.

Exit codes are the point: this is meant to run in CI and from a doctor flow,
where a release that cannot be installed must be a red rather than a surprise
in somebody else's build a day later.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runVerify,
}

func init() {
	verifyCmd.Flags().String("dir", "", "manifest directory (default: cwd)")
	Cmd.AddCommand(verifyCmd)
}

func runVerify(c *cobra.Command, args []string) error {
	dir, _ := c.Flags().GetString("dir")
	manifest, workDir, err := loadManifest(dir)
	if err != nil {
		return err
	}
	version := manifest.Version.String()
	if len(args) == 1 {
		version = strings.TrimPrefix(args[0], "v")
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseVerifyBudget)
	defer cancel()
	if err := Verify(ctx, manifest, workDir, version); err != nil {
		if errors.Is(err, errReleaseInFlight) {
			cli.Info("%v", err)
			return nil
		}
		return fmt.Errorf("verify failed: %w", err)
	}
	cli.Info("v%s is published, installable through the agent loader, and advertises a contract this CLI can speak", version)
	return nil
}

// Verify establishes one release. It is the single mechanism: `codefly publish`
// calls verifyPublication below after it pushes a tag, and this command calls
// the same function. Before this existed the two publication owners were
// verified to different strengths by the same command — a workflow-published
// release had its bytes checked against an independent digest and a
// cli-published one did not.
func Verify(ctx context.Context, manifest *Manifest, workDir, version string) error {
	tag := "v" + version
	client, err := gh.NewClient()
	if err != nil {
		return err
	}
	owner, repo, err := originRepository(ctx, workDir)
	if err != nil {
		return err
	}
	if err := verifyTagLanded(ctx, client, workDir, owner, repo, manifest, tag); err != nil {
		return err
	}
	cli.Info("%s names a commit %s/%s's main carries", tag, owner, repo)

	if manifest.Mode != ModeAgent {
		// core, cli and standalone modules are consumed as Go modules at their
		// tag; the tag IS the release and there is no artefact to download.
		cli.Info("%s releases are consumed at the tag, so there are no published assets to read", manifest.Mode)
		return nil
	}
	identity, err := readAgentIdentity(manifest.Path)
	if err != nil {
		return err
	}
	switch {
	case sourceTagKinds[identity.Kind]:
		cli.Info("%s agents publish only the signed tag, so the tag check is the whole verification", identity.Kind)
		return nil
	case !loaderAssetKinds[identity.Kind]:
		return unsupportedReleaseKindError(identity.Kind)
	}
	target, err := newReleaseTarget(identity, version)
	if err != nil {
		return err
	}
	if err := verifyPublication(ctx, client, target); err != nil {
		return err
	}
	return verifyCachedBytesMatch(ctx, target)
}

// releaseTarget names everything a verification needs about one release:
// where it lives and which assets it must carry.
type releaseTarget struct {
	reg       *resources.AgentKindRegistration
	publisher string
	name      string
	version   string
	tag       string
	assets    []loaderAsset
}

func newReleaseTarget(identity agentIdentity, version string) (releaseTarget, error) {
	kind := identity.Kind
	if kind == "" {
		kind = string(resources.ServiceAgent)
	}
	reg, err := resources.AgentKindRegistrationFor(resources.AgentKind(kind))
	if err != nil {
		return releaseTarget{}, err
	}
	return releaseTarget{
		reg:       &reg,
		publisher: identity.Publisher,
		name:      identity.Name,
		version:   version,
		tag:       "v" + version,
		assets:    expectedLoaderAssets(&reg, identity.Name, version),
	}, nil
}

func (t releaseTarget) owner() string { return gh.Owner(t.publisher) }
func (t releaseTarget) repo() string  { return t.reg.GitHubRepository(t.name) }

func (t releaseTarget) agent() *resources.Agent {
	return &resources.Agent{Kind: t.reg.Resource, Publisher: t.publisher, Name: t.name, Version: t.version}
}

// verifyPublication is the verification both entry points run against a live
// release. It reads only the published artefact, so it establishes the same
// facts whoever published it and whenever it is asked.
func verifyPublication(ctx context.Context, client *github.Client, t releaseTarget) error {
	if err := verifyPublishedArchives(ctx, client, t.owner(), t.repo(), t.tag, t.assets); err != nil {
		return err
	}
	if err := verifyReleaseAssets(ctx, client, t.reg, t.publisher, t.name, t.version, t.assets); err != nil {
		return err
	}
	if err := verifyInstallCredential(ctx, t); err != nil {
		return err
	}
	return verifyAdvertisedContract(ctx, t)
}

// errReleaseInFlight marks a version whose tag is legitimately not there yet: a
// live `codefly publish` holds the tag unpushed while main's post-merge checks
// run, and pushes it when they go green. That window is correct behaviour and
// reporting it as a missing release would train people to ignore this command.
var errReleaseInFlight = errors.New("release is still in flight")

// verifyTagLanded asks whether the version main declares is named by a tag
// whose commit main carries.
//
// main is read through the API rather than from the checkout. The local main
// can be far behind, and `publish verify` is meant to be runnable from any
// checkout — including one that has never fetched.
func verifyTagLanded(ctx context.Context, client *github.Client, workDir, owner, repo string, manifest *Manifest, tag string) error {
	mainVersion, err := declaredVersionOnMain(ctx, client, workDir, owner, repo, manifest)
	if err != nil {
		return err
	}
	commit, err := tagCommit(ctx, client, owner, repo, tag)
	switch {
	case err != nil && !errors.Is(err, errNoSuchTag):
		return err
	case errors.Is(err, errNoSuchTag) && "v"+mainVersion != tag:
		return fmt.Errorf("no tag %s exists (main declares %s)", tag, mainVersion)
	case errors.Is(err, errNoSuchTag):
		return untaggedMainVerdict(ctx, workDir, owner, repo, tag)
	}
	branch, _, err := client.Repositories.GetBranch(ctx, owner, repo, "main", 0)
	if err != nil {
		return fmt.Errorf("read %s/%s main: %w", owner, repo, err)
	}
	head := branch.GetCommit().GetSHA()
	if head == commit {
		return nil
	}
	comparison, _, err := client.Repositories.CompareCommits(ctx, owner, repo, commit, head, nil)
	if err != nil {
		return fmt.Errorf("compare %s with main: %w", tag, err)
	}
	// base=tag, head=main: "ahead" means main has moved on from the tag's
	// commit, which is the normal state of a released repository.
	if status := comparison.GetStatus(); status != "ahead" && status != "identical" {
		return fmt.Errorf("%s names commit %s, which main does not carry (main is %s of it); the tag does not name what shipped",
			tag, commit[:12], status)
	}
	return nil
}

// untaggedMainVerdict separates the two reasons main can declare a version no
// tag names. A publish in flight is holding the tag until main's checks go
// green; anything else means the bump landed and the tag never followed, which
// is the window `codefly publish` can resume but nothing ever detects.
func untaggedMainVerdict(ctx context.Context, workDir, owner, repo, tag string) error {
	landing, err := newPullRequestLanding(ctx, workDir)
	if err != nil {
		return err
	}
	branch, _, err := landing.client.Repositories.GetBranch(ctx, owner, repo, "main", 0)
	if err != nil {
		return fmt.Errorf("read %s/%s main: %w", owner, repo, err)
	}
	head := branch.GetCommit().GetSHA()
	verdict, err := landing.inspectChecks(ctx, head)
	if err != nil {
		return fmt.Errorf("read checks on main (%s): %w", head[:12], err)
	}
	switch {
	case len(verdict.failed) > 0:
		return fmt.Errorf("main declares %s but its checks are red (%s), so no tag has been pushed for it; fix CI and re-run publish",
			strings.TrimPrefix(tag, "v"), strings.Join(verdict.failed, ", "))
	case !verdict.ready:
		return fmt.Errorf("%w: main declares %s and its checks are still running at %s, which is when a live publish holds the tag back",
			errReleaseInFlight, strings.TrimPrefix(tag, "v"), head[:12])
	}
	return fmt.Errorf("main declares %s at %s with every check green, but no tag names it: the bump landed and the tag never followed; re-run `codefly publish` in a checkout of main to finish it",
		strings.TrimPrefix(tag, "v"), head[:12])
}

// declaredVersionOnMain reads the manifest as main holds it. The local file
// would answer about this checkout, which is the mistake this command exists
// to stop making.
func declaredVersionOnMain(ctx context.Context, client *github.Client, workDir, owner, repo string, manifest *Manifest) (string, error) {
	relative, err := filepath.Rel(workDir, manifest.Path)
	if err != nil {
		return "", fmt.Errorf("locate %s inside %s: %w", manifest.Path, workDir, err)
	}
	contents, _, _, err := client.Repositories.GetContents(ctx, owner, repo, filepath.ToSlash(relative),
		&github.RepositoryContentGetOptions{Ref: "main"})
	if err != nil {
		return "", fmt.Errorf("read %s from %s/%s main: %w", relative, owner, repo, err)
	}
	raw, err := contents.GetContent()
	if err != nil {
		return "", fmt.Errorf("decode %s from main: %w", relative, err)
	}
	version, err := parseVersionField([]byte(raw))
	if err != nil {
		return "", fmt.Errorf("read the version main declares in %s: %w", relative, err)
	}
	return version.String(), nil
}

var errNoSuchTag = errors.New("no such tag")

// tagCommit resolves a tag to the commit it names, dereferencing an annotated
// tag object. codefly tags are signed and therefore annotated, so the
// dereference is the normal path rather than the exception.
func tagCommit(ctx context.Context, client *github.Client, owner, repo, tag string) (string, error) {
	ref, response, err := client.Git.GetRef(ctx, owner, repo, "tags/"+tag)
	if err != nil {
		if response != nil && response.StatusCode == 404 {
			return "", errNoSuchTag
		}
		return "", fmt.Errorf("read tag %s: %w", tag, err)
	}
	switch ref.GetObject().GetType() {
	case "commit":
		return ref.GetObject().GetSHA(), nil
	case "tag":
		object, _, err := client.Git.GetTag(ctx, owner, repo, ref.GetObject().GetSHA())
		if err != nil {
			return "", fmt.Errorf("dereference annotated tag %s: %w", tag, err)
		}
		return object.GetObject().GetSHA(), nil
	default:
		return "", fmt.Errorf("tag %s names a %s, not a commit", tag, ref.GetObject().GetType())
	}
}

// verifyInstallCredential opens the host platform's archive with the credential
// the install path itself resolves, which is not the one publish has always
// verified with.
//
// `gh.Token()` falls back to the `gh` CLI's stored login. Core's
// openAgentRelease takes its authenticated path only when GITHUB_TOKEN or
// GH_TOKEN is set, and otherwise does a plain unauthenticated GET that a
// private repository answers with 404. So a release verified only through
// gh.Token() goes green on a laptop and 404s in the CI and hosted builds that
// install it — which is a failure that has already cost a hosted render.
//
// The env-only resolution here deliberately mirrors core's unexported
// githubReleaseToken rather than reusing gh.Token(). It must stay a mirror:
// codefly-dev/core should export that rule so there is one copy of it.
func verifyInstallCredential(ctx context.Context, t releaseTarget) error {
	host := platform{os: runtime.GOOS, arch: runtime.GOARCH}
	url := loaderDownloadURL(t.reg, t.publisher, t.name, t.version, host)
	token := installPathToken()
	if token == "" {
		return fmt.Errorf("neither GITHUB_TOKEN nor GH_TOKEN is set, so this verification cannot establish that %s is reachable by the credential the agent loader uses; a gh CLI login does not count, because core's downloader never reads it", url)
	}
	client, err := github.NewClient(github.WithAuthToken(token))
	if err != nil {
		return fmt.Errorf("build a client with the install path's credential: %w", err)
	}
	body, _, err := manager.OpenReleaseAsset(ctx, url, client)
	if err != nil {
		return fmt.Errorf("%s is not reachable with the credential the agent loader uses (GITHUB_TOKEN/GH_TOKEN): %w", url, err)
	}
	return body.Close()
}

// installPathToken mirrors core's githubReleaseToken: GITHUB_TOKEN or GH_TOKEN,
// and never the gh CLI. See verifyInstallCredential for why it is a mirror.
func installPathToken() string {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			return token
		}
	}
	return ""
}

// verifyAdvertisedContract installs the published archive into a throwaway
// agent base and asks the agent what it advertises.
//
// It asks rather than inspects. Counting a capability string inside the binary
// would answer about a name appearing in a file, which is the whole class of
// false green this verification exists to close: the only thing that
// establishes what an agent advertises is the agent advertising it. The
// compatibility rule applied to the answer is core's own contract.Check, so
// this says exactly what a host would say when it loads the released agent.
//
// The base is a temporary directory, so this never consults — or writes — the
// machine's real agent cache, and a release that is already cached locally is
// still downloaded and run.
func verifyAdvertisedContract(ctx context.Context, t releaseTarget) error {
	base, err := os.MkdirTemp("", "codefly-publish-verify-base-*")
	if err != nil {
		return fmt.Errorf("create a throwaway agent base: %w", err)
	}
	defer os.RemoveAll(base)
	restore, err := useAgentBase(base)
	if err != nil {
		return err
	}
	defer restore()
	defer services.ClearAgents()

	agent := t.agent()
	if err := manager.Download(ctx, agent); err != nil {
		return fmt.Errorf("the published release does not install through the agent loader: %w", err)
	}
	loaded, err := services.LoadAgent(ctx, agent, "")
	if err != nil {
		return fmt.Errorf("the published release installs but the agent does not load: %w", err)
	}
	info, err := loaded.GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})
	if err != nil {
		return fmt.Errorf("the loaded agent does not answer what it advertises: %w", err)
	}
	advertised := info.GetContract()
	if err := contract.Check(advertised); err != nil {
		return fmt.Errorf("the released agent advertises a contract this CLI cannot speak: %w", err)
	}
	cli.Info("%s/%s@%s advertises protocol %d with capabilities [%s]",
		t.publisher, t.name, t.version, advertised.GetProtocolVersion(), strings.Join(advertised.GetCapabilities(), " "))
	return nil
}

// useAgentBase points the agent loader at base for the duration of one check
// and returns the restore. CODEFLY_HOME is the only seam core offers for this —
// resources.AgentBase reads the process environment, not the context — so this
// is process-wide while it is set. `codefly publish verify` does one thing at a
// time, which is what makes that safe here and not in a concurrent caller.
func useAgentBase(base string) (func(), error) {
	previous, had := os.LookupEnv(resources.CodeflyHomeEnv)
	if err := os.Setenv(resources.CodeflyHomeEnv, base); err != nil {
		return nil, fmt.Errorf("point the agent loader at a throwaway base: %w", err)
	}
	return func() {
		if had {
			_ = os.Setenv(resources.CodeflyHomeEnv, previous)
			return
		}
		_ = os.Unsetenv(resources.CodeflyHomeEnv)
	}, nil
}

// verifyCachedBytesMatch compares the machine's cached agent for this version
// with the bytes the release publishes.
//
// The cache is keyed by name: `<name>__<version>`. Nothing has ever compared
// its contents with the release it is named after, and core's downloader
// verifies no checksum on install, so a local build left under a released
// version's name is run by every sibling that resolves that version — silently,
// and with no way to tell from any output that it is not the release.
//
// A mismatch is a verdict about this machine rather than about the release, so
// it is reported separately from verifyPublication and names the remedy.
func verifyCachedBytesMatch(ctx context.Context, t releaseTarget) error {
	cached, err := t.agent().Path(ctx)
	if err != nil {
		return fmt.Errorf("locate the cached agent: %w", err)
	}
	local, err := fileDigest(cached)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the cached agent %s: %w", cached, err)
	}
	published, err := publishedBinaryDigest(ctx, t)
	if err != nil {
		return err
	}
	if local != published {
		return fmt.Errorf("the cached agent %s is sha256:%s but %s publishes sha256:%s — this machine runs a different build under the released version's name; delete that file and it will be re-downloaded from the release",
			cached, local[:16], t.tag, published[:16])
	}
	cli.Info("the cached %s__%s matches the published release byte for byte", t.name, t.version)
	return nil
}

// publishedBinaryDigest downloads the release into a throwaway base and digests
// the binary the loader would install, which is what the cache holds — the
// extracted executable, not the archive.
func publishedBinaryDigest(ctx context.Context, t releaseTarget) (string, error) {
	base, err := os.MkdirTemp("", "codefly-publish-verify-cache-*")
	if err != nil {
		return "", fmt.Errorf("create a throwaway agent base: %w", err)
	}
	defer os.RemoveAll(base)
	restore, err := useAgentBase(base)
	if err != nil {
		return "", err
	}
	defer restore()
	agent := t.agent()
	if err := manager.Download(ctx, agent); err != nil {
		return "", fmt.Errorf("download the published release: %w", err)
	}
	installed, err := agent.Path(ctx)
	if err != nil {
		return "", err
	}
	return fileDigest(installed)
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // path is composed from the agent resolver
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}
