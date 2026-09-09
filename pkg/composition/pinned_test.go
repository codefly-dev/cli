package composition

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/codefly-dev/cli/pkg/composition/pinnedfixture"
	corecomposition "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The fixture serving a real signed module-package release lives in
// pinnedfixture so the `run`-side materialization tests exercise the same
// verification path; these aliases keep the tests below reading in this
// package's own vocabulary.
const (
	testOwner     = pinnedfixture.Owner
	testRepoName  = pinnedfixture.RepoName
	testPackageID = pinnedfixture.PackageID
	testCommit    = pinnedfixture.Commit
	testSigner    = pinnedfixture.Signer
)

func testRepositoryURL() string { return pinnedfixture.RepositoryURL() }

func newPinnedFixture(t *testing.T) *pinnedfixture.Fixture { return pinnedfixture.New(t) }

func writeWorkspace(t *testing.T, dir string, fixture *pinnedfixture.Fixture) {
	pinnedfixture.WriteWorkspace(t, dir, fixture)
}

func useFixtureGitHub(t *testing.T, fixture *pinnedfixture.Fixture) { fixture.UseGitHub(t) }

func allowTempDirCleanup(t *testing.T, root string) { pinnedfixture.AllowTempDirCleanup(t, root) }

// removeReadOnlyTree deletes root, a materialized module cache directory the
// Materializer chmods read-only (0o555) after promotion — plain os.RemoveAll
// cannot unlink entries inside a directory it has no write permission on.
func removeReadOnlyTree(root string) error {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o755)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// A workspace author writing the repository with a trailing ".git" (the
// exact form PinnedSourceURL itself produces for a bare "owner/repo" source
// elsewhere in this codebase) must still verify successfully: the producer's
// provenance.Repository never carries ".git", so an un-normalized trust
// config would resolve the package identity (matching loosely) and then fail
// VerifyRelease's byte-for-byte comparison (matching strictly) — a
// self-contradictory, confusing failure for what is really the same
// repository.
func TestResolvePinnedModuleAcceptsGitSuffixedTrustRepository(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	fixture := newPinnedFixture(t)
	fixture.AddRelease(t, "0.1.0")
	useFixtureGitHub(t, fixture)

	workspaceDir := t.TempDir()
	allowTempDirCleanup(t, workspaceDir)
	doc := fmt.Sprintf(`name: wiki
layout: modules
module-trust:
  repositories:
    %s: %s.git
  signers:
    %s: %q
`, testPackageID, testRepositoryURL(), testSigner, fixture.SignerKeyBase64())
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDir, resources.WorkspaceConfigurationName), []byte(doc), 0o644))
	ref := &resources.ModuleReference{Name: "saas", Source: testOwner + "/" + testRepoName, Version: "0.1.0"}

	resolved, err := ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(resolved.Dir, corecomposition.PackageManifestFileName))
}

func TestResolvePinnedModuleVerifiesAndMaterializes(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	fixture := newPinnedFixture(t)
	fixture.AddRelease(t, "0.1.0")
	useFixtureGitHub(t, fixture)

	workspaceDir := t.TempDir()
	allowTempDirCleanup(t, workspaceDir)
	writeWorkspace(t, workspaceDir, fixture)
	ref := &resources.ModuleReference{Name: "saas", Source: testOwner + "/" + testRepoName, Version: "0.1.0"}

	resolved, err := ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(resolved.Dir, corecomposition.PackageManifestFileName))
	require.Equal(t, testPackageID, resolved.Package)
	require.Equal(t, "0.1.0", resolved.Version)

	requestsAfterFirst := fixture.Requests()
	resolved2, err := ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.NoError(t, err)
	require.Equal(t, resolved.Dir, resolved2.Dir)
	require.Equal(t, requestsAfterFirst, fixture.Requests(), "a cached exact version must not hit the network again")
}

// A `module:` field naming a subpath the package doesn't actually contain
// must fail immediately with a clear error, both when the package is fetched
// fresh and when it is served from the no-network cache hit — not succeed
// with a PinnedResolution pointing at a directory that doesn't exist, which
// would only surface as a much less specific failure later when `run`
// actually tries to load the module.
func TestResolvePinnedModuleRejectsMissingModuleSubpath(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	fixture := newPinnedFixture(t)
	fixture.AddRelease(t, "0.1.0")
	useFixtureGitHub(t, fixture)

	workspaceDir := t.TempDir()
	allowTempDirCleanup(t, workspaceDir)
	writeWorkspace(t, workspaceDir, fixture)
	ref := &resources.ModuleReference{Name: "saas", Source: testOwner + "/" + testRepoName, Version: "0.1.0", Module: "does-not-exist"}

	_, err := ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.ErrorContains(t, err, "module subpath")

	// The package itself verified and materialized fine — only ref.Module is
	// wrong — so the second attempt must raise the same error from the
	// no-network cache-hit path rather than re-fetching from GitHub.
	requestsAfterFirst := fixture.Requests()
	_, err = ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.ErrorContains(t, err, "module subpath")
	require.Equal(t, requestsAfterFirst, fixture.Requests(), "the cached, already-verified package must not be re-fetched")
}

// The no-network fast path must not trust a cached directory on manifest
// identity alone: content modified after materialization (bypassing the
// read-only permissions Materialize leaves the tree with, exactly as
// corruption or a local compromise would) must be detected and force a real
// re-verification, restoring the genuine content, rather than being served
// silently forever.
func TestResolvePinnedModuleDetectsCacheTampering(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	fixture := newPinnedFixture(t)
	fixture.AddRelease(t, "0.1.0")
	useFixtureGitHub(t, fixture)

	workspaceDir := t.TempDir()
	allowTempDirCleanup(t, workspaceDir)
	writeWorkspace(t, workspaceDir, fixture)
	ref := &resources.ModuleReference{Name: "saas", Source: testOwner + "/" + testRepoName, Version: "0.1.0"}

	resolved, err := ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.NoError(t, err)

	tamperedFile := filepath.Join(resolved.Dir, "services", "frontend.txt")
	require.NoError(t, os.Chmod(filepath.Dir(tamperedFile), 0o755))
	require.NoError(t, os.Chmod(tamperedFile, 0o644))
	require.NoError(t, os.WriteFile(tamperedFile, []byte("tampered"), 0o644))

	requestsBeforeSecond := fixture.Requests()
	resolved2, err := ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.NoError(t, err)
	require.Greater(t, fixture.Requests(), requestsBeforeSecond,
		"a tampered cache entry must not be trusted without a network re-verification")

	content, err := os.ReadFile(filepath.Join(resolved2.Dir, "services", "frontend.txt"))
	require.NoError(t, err)
	require.Equal(t, "frontend", string(content), "the tampered file must be restored to the verified content")
}

func TestResolvePinnedModuleRejectsUntrustedSigner(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	fixture := newPinnedFixture(t)
	fixture.AddRelease(t, "0.1.0")
	useFixtureGitHub(t, fixture)

	workspaceDir := t.TempDir()
	doc := fmt.Sprintf(`name: wiki
layout: modules
module-trust:
  repositories:
    %s: %s
  signers: {}
`, testPackageID, testRepositoryURL())
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDir, resources.WorkspaceConfigurationName), []byte(doc), 0o644))
	ref := &resources.ModuleReference{Name: "saas", Source: testOwner + "/" + testRepoName, Version: "0.1.0"}

	_, err := ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.Error(t, err)
	require.ErrorIs(t, err, corecomposition.ErrSignature)

	_, statErr := os.Stat(filepath.Join(workspaceDir, ".codefly", "cache", "modules"))
	require.True(t, os.IsNotExist(statErr), "an untrusted signature must materialize nothing")
}

func TestResolvePinnedModuleRejectsMovedTag(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	fixture := newPinnedFixture(t)
	fixture.AddRelease(t, "0.1.0")
	useFixtureGitHub(t, fixture)

	workspaceDir := t.TempDir()
	allowTempDirCleanup(t, workspaceDir)
	writeWorkspace(t, workspaceDir, fixture)
	ref := &resources.ModuleReference{Name: "saas", Source: testOwner + "/" + testRepoName, Version: "0.1.0"}

	// A first resolve succeeds and records the release's digest and peeled
	// commit in the resolved index.
	first, err := ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.NoError(t, err)

	// The remote re-tags module-package/v0.1.0 at an entirely different,
	// internally self-consistent commit and content. Evicting only the cached
	// directory (not the resolved index) forces a network re-fetch while
	// keeping the previously recorded digest/commit to compare against.
	fixture.AddReleaseAt(t, "0.1.0", strings.Repeat("b", 40), "retagged")
	cacheDir, err := corecomposition.NewMaterializer(workspaceDir).CachePath(first.Digest)
	require.NoError(t, err)
	require.NoError(t, removeReadOnlyTree(cacheDir))

	_, err = ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.Error(t, err)
	require.ErrorIs(t, err, corecomposition.ErrMovedTag)
}

// A plain release sharing the same "v*" tag namespace (a deploy-counter tag,
// carrying no module-package assets) must never be picked as a candidate
// module-package version, even when its version number would otherwise
// satisfy the constraint and numerically outrank every real candidate.
func TestResolvePinnedModuleConstraintPicksHighestPackageTrack(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	fixture := newPinnedFixture(t)
	fixture.AddRelease(t, "0.1.0")
	fixture.AddRelease(t, "0.2.0")
	fixture.AddPlainRelease("v0.5.0")
	useFixtureGitHub(t, fixture)

	workspaceDir := t.TempDir()
	allowTempDirCleanup(t, workspaceDir)
	writeWorkspace(t, workspaceDir, fixture)
	ref := &resources.ModuleReference{Name: "saas", Source: testOwner + "/" + testRepoName, Version: ">=0.1.0"}

	resolved, err := ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.NoError(t, err)
	require.Equal(t, "0.2.0", resolved.Version)
}

// Two concurrent resolutions of *different* package versions must not lose
// either one's entry: a naive read-whole-map/set-one-key/write-whole-map
// cycle with no locking would let the second writer's full-map write clobber
// whatever the first had just added, silently disabling moved-tag detection
// for the lost entry.
func TestWriteResolvedIndexConcurrentWritesDoNotLoseEntries(t *testing.T) {
	root := t.TempDir()
	const count = 20
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			packageID := fmt.Sprintf("owner/pkg%d", i)
			err := writeResolvedIndex(context.Background(), root, packageID, "1.0.0", resolvedEntry{
				Digest: fmt.Sprintf("sha256:%064d", i),
				Commit: strings.Repeat("a", 40),
			})
			require.NoError(t, err)
		}(i)
	}
	wg.Wait()

	for i := range count {
		packageID := fmt.Sprintf("owner/pkg%d", i)
		entry, ok := readResolvedIndex(root, packageID, "1.0.0")
		if !ok {
			t.Fatalf("entry for %s@1.0.0 was lost to a concurrent write", packageID)
		}
		require.Equal(t, fmt.Sprintf("sha256:%064d", i), entry.Digest)
	}
}

// Some producers publish the module-package track under a literal
// "module-package/v"+version tag rather than a plain "v"+version tag with the
// package assets attached — GitHubResolver.Resolve can only ever fetch the
// latter, so ResolvePinnedModule must still succeed against the former via
// its Fetch fallback, both for an exact pin and for constraint/latest
// resolution (which must also list literally-prefixed releases as
// candidates).
func TestResolvePinnedModuleFetchesModulePackagePrefixedTag(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	fixture := newPinnedFixture(t)
	fixture.AddPrefixedRelease(t, "0.3.0", testCommit, "prefixed")
	useFixtureGitHub(t, fixture)

	workspaceDir := t.TempDir()
	allowTempDirCleanup(t, workspaceDir)
	writeWorkspace(t, workspaceDir, fixture)
	ref := &resources.ModuleReference{Name: "saas", Source: testOwner + "/" + testRepoName, Version: "0.3.0"}

	resolved, err := ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(resolved.Dir, corecomposition.PackageManifestFileName))
}

func TestResolvePinnedModuleConstraintListsModulePackagePrefixedTags(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	fixture := newPinnedFixture(t)
	fixture.AddRelease(t, "0.1.0")
	fixture.AddPrefixedRelease(t, "0.4.0", testCommit, "prefixed")
	useFixtureGitHub(t, fixture)

	workspaceDir := t.TempDir()
	allowTempDirCleanup(t, workspaceDir)
	writeWorkspace(t, workspaceDir, fixture)
	ref := &resources.ModuleReference{Name: "saas", Source: testOwner + "/" + testRepoName, Version: ">=0.1.0"}

	resolved, err := ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.NoError(t, err)
	require.Equal(t, "0.4.0", resolved.Version)
}

// A moved tag must be caught the first time a *different* workspace (or, in
// practice, a different git worktree of the same solution) resolves the same
// package version — not just in the workspace that happened to resolve it
// first — since the resolved-version index is global rather than scoped to
// the materializer's per-workspace cache directory.
func TestResolvePinnedModuleMovedTagDetectedAcrossWorkspaces(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	fixture := newPinnedFixture(t)
	fixture.AddRelease(t, "0.1.0")
	useFixtureGitHub(t, fixture)

	firstWorkspace := t.TempDir()
	allowTempDirCleanup(t, firstWorkspace)
	writeWorkspace(t, firstWorkspace, fixture)
	ref := &resources.ModuleReference{Name: "saas", Source: testOwner + "/" + testRepoName, Version: "0.1.0"}
	_, err := ResolvePinnedModule(context.Background(), firstWorkspace, ref)
	require.NoError(t, err)

	// The remote re-tags v0.1.0 at a different, internally self-consistent
	// commit and content.
	fixture.AddReleaseAt(t, "0.1.0", strings.Repeat("b", 40), "retagged")

	secondWorkspace := t.TempDir()
	allowTempDirCleanup(t, secondWorkspace)
	writeWorkspace(t, secondWorkspace, fixture)
	_, err = ResolvePinnedModule(context.Background(), secondWorkspace, ref)
	require.Error(t, err)
	require.ErrorIs(t, err, corecomposition.ErrMovedTag)
}

// The unverified-clone choice must come from an explicit statement — the user's
// directive or the record — never from what the resolved path happens to look
// like. Inferring it from a cache-root prefix silently downgraded a verified
// module to an unverified clone whenever the workspace itself sat inside that
// cache root (a materialized module repo is a workspace, so `run` inside one is
// a real layout).
func TestGitResolutionFor(t *testing.T) {
	cacheShapedPath := filepath.Join(t.TempDir(), ".codefly", "modules", "owner", "repo", "v1.0.0")
	for _, tc := range []struct {
		name      string
		directive *resources.ModuleResolveDirective
		recorded  *ResolutionReceipt
		want      bool
	}{
		{name: "bare reference, nothing recorded"},
		{name: "bare reference, recorded", recorded: &ResolutionReceipt{Mode: ResolutionModeGit}, want: true},
		{
			name:      "git opts in",
			directive: &resources.ModuleResolveDirective{Git: true},
			want:      true,
		},
		{
			name:      "pinned revokes a recorded opt-in",
			directive: &resources.ModuleResolveDirective{Pinned: true},
			recorded:  &ResolutionReceipt{Mode: ResolutionModeGit},
		},
		{
			name:      "cache-shaped path is not by itself an opt-in",
			directive: &resources.ModuleResolveDirective{Path: cacheShapedPath},
		},
		{
			name:      "recorded clone path stays a clone",
			directive: &resources.ModuleResolveDirective{Path: cacheShapedPath},
			recorded:  &ResolutionReceipt{Mode: ResolutionModeGit, Path: cacheShapedPath},
			want:      true,
		},
		{
			name:      "user checkout is not recorded, so not a clone",
			directive: &resources.ModuleResolveDirective{Path: "/home/me/checkout"},
		},
		{
			name:      "a verified receipt keeps the module verified",
			directive: &resources.ModuleResolveDirective{Path: cacheShapedPath},
			recorded:  &ResolutionReceipt{Mode: ResolutionModeVerified, Path: cacheShapedPath},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GitResolutionFor(tc.directive, tc.recorded); got != tc.want {
				t.Fatalf("GitResolutionFor = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolutionRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()

	absent, err := LoadResolutionReceipts(dir)
	require.NoError(t, err, "an absent record is the normal case")
	require.Empty(t, absent)

	receipts := map[string]*ResolutionReceipt{
		"saas": {
			Source: "owner/saas", Requested: "v1.0.0", Mode: ResolutionModeVerified,
			Version: "1.0.0", Path: "/cache/saas/v1", Digest: "sha256:abc", Commit: "deadbeef",
		},
		"documents": {
			Source: "owner/documents", Module: "module", Requested: "latest",
			Mode: ResolutionModeGit, Version: "v2.0.0", Path: "/cache/documents/v2",
		},
	}
	require.NoError(t, SaveResolutionReceipts(context.Background(), dir, receipts))
	loaded, err := LoadResolutionReceipts(dir)
	require.NoError(t, err)
	require.Equal(t, receipts, loaded, "a receipt binds the whole request, not just the path")

	// Malformed is an error, never an empty set: read as empty it would resolve
	// an opted-out module through the verified package and report the failure as
	// missing module-trust, and would reclassify every machine path as a user
	// checkout.
	require.NoError(t, os.WriteFile(filepath.Join(dir, ResolutionRecordName), []byte("resolved: [\n"), 0o600))
	_, err = LoadResolutionReceipts(dir)
	require.Error(t, err)
}

// A record written by a CLI that predates receipts still identifies its module
// as git-resolved and its path as machine output; it just cannot claim to answer
// any request until the module is re-materialized.
func TestLoadResolutionReceiptsMigratesGitResolvedRecord(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ResolutionRecordName),
		[]byte("git-resolved:\n    saas: /cache/saas/v0.0.1\n"), 0o600))

	receipts, err := LoadResolutionReceipts(dir)
	require.NoError(t, err)
	require.Equal(t, ResolutionModeGit, receipts["saas"].Mode)
	require.Equal(t, "/cache/saas/v0.0.1", receipts["saas"].Path)
	require.True(t, GitResolutionFor(nil, receipts["saas"]), "the opt-out must survive the upgrade")

	ref := &resources.ModuleReference{Name: "saas", Source: "owner/saas", Version: "v0.0.1"}
	require.False(t, receipts["saas"].Answers(ref, ResolutionModeGit),
		"a migrated record records no request, so it answers none")
}

// A receipt is eligible only for the request it was produced for: any change to
// the source, module subpath, requested version or materialization mode makes it
// unable to vouch for its path.
func TestResolutionReceiptAnswers(t *testing.T) {
	ref := &resources.ModuleReference{Name: "saas", Source: "owner/saas", Module: "module", Version: "v0.0.1"}
	receipt := &ResolutionReceipt{
		Source: "owner/saas", Module: "module", Requested: "v0.0.1",
		Mode: ResolutionModeGit, Version: "v0.0.1", Path: "/cache/saas/v0.0.1",
	}
	require.True(t, receipt.Answers(ref, ResolutionModeGit))
	require.False(t, receipt.Answers(ref, ResolutionModeVerified), "mode is part of the request")

	bumped := *ref
	bumped.Version = "v9.9.9"
	require.False(t, receipt.Answers(&bumped, ResolutionModeGit), "a bumped version is a new request")

	moved := *ref
	moved.Source = "owner/other"
	require.False(t, receipt.Answers(&moved, ResolutionModeGit), "a changed source is a new request")

	subpath := *ref
	subpath.Module = "other"
	require.False(t, receipt.Answers(&subpath, ResolutionModeGit), "a changed module subpath is a new request")

	// An absent version and an explicit `latest` are the same request spelled
	// two ways, so switching between them must not invalidate the receipt.
	latest := &resources.ModuleReference{Name: "saas", Source: "owner/saas", Version: ""}
	latestReceipt := &ResolutionReceipt{
		Source: "owner/saas", Requested: "latest", Mode: ResolutionModeGit,
		Version: "v0.1.0", Path: "/cache/saas/v0.1.0",
	}
	require.True(t, latestReceipt.Answers(latest, ResolutionModeGit))
}
