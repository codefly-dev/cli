package composition

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	corecomposition "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const (
	testOwner     = "codefly-dev"
	testRepoName  = "module-saas-starter"
	testPackageID = "codefly/saas-starter"
	testSigner    = "https://github.com/codefly-dev/module-saas-starter/.github/workflows/release.yml@refs/heads/main"
	testCommit    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func testRepositoryURL() string {
	return fmt.Sprintf("https://github.com/%s/%s", testOwner, testRepoName)
}

// pinnedFixture is a GitHub API + release-asset test double serving one
// signed module-package release per registered version, replicating core's
// composition_test.go httptest harness (fixtureResolver /
// TestGitHubResolverFetchesAssetsAndPeeledCommit) against the exported
// composition API this package builds on.
type pinnedFixture struct {
	server      *httptest.Server
	privateKey  ed25519.PrivateKey
	publicKey   ed25519.PublicKey
	releases    map[string]releaseAssets // tag -> assets
	plainTags   []string                 // deploy-counter tags with no module-package assets
	assetsByID  map[int64][]byte
	nextAssetID int64
	requests    atomic.Int64
}

type releaseAssets struct {
	commit                                     string
	archiveID, provenanceID, signatureID       int64
	archiveSize, provenanceSize, signatureSize int
}

func newPinnedFixture(t *testing.T) *pinnedFixture {
	t.Helper()
	seed := strings.Repeat("k", ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed([]byte(seed))
	fixture := &pinnedFixture{
		privateKey: privateKey,
		publicKey:  privateKey.Public().(ed25519.PublicKey),
		releases:   map[string]releaseAssets{},
		assetsByID: map[int64][]byte{},
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.handle))
	t.Cleanup(fixture.server.Close)
	return fixture
}

// addRelease registers a module-package release tagged v<version>, signed and
// digest-matched for immediate verification, at testCommit.
func (fixture *pinnedFixture) addRelease(t *testing.T, version string) {
	t.Helper()
	fixture.addReleaseAt(t, version, testCommit, "frontend")
}

// addPlainRelease registers a release at tag with no module-package assets —
// the deploy-counter track sharing the same "v*" tag namespace, which must
// never be picked as a candidate module-package version.
func (fixture *pinnedFixture) addPlainRelease(tag string) {
	fixture.plainTags = append(fixture.plainTags, tag)
}

// addReleaseAt (re)registers tag "v"+version as a fresh, internally
// self-consistent release at commit — different content so a different commit
// also means a different artifact digest, mirroring what an actual re-tag
// would republish. Used to simulate the remote moving an immutable tag.
func (fixture *pinnedFixture) addReleaseAt(t *testing.T, version, commit, content string) {
	t.Helper()
	fixture.addReleaseTagged(t, "v"+version, version, commit, content)
}

// addPrefixedRelease registers a release under the literal
// "module-package/v"+version tag some producers use instead of a plain
// "v"+version tag, exercising fetchPackageRelease's Fetch fallback (Resolve
// can only ever fetch a plain "v"+version tag).
func (fixture *pinnedFixture) addPrefixedRelease(t *testing.T, version, commit, content string) {
	t.Helper()
	fixture.addReleaseTagged(t, modulePackageTagPrefix+version, version, commit, content)
}

func (fixture *pinnedFixture) addReleaseTagged(t *testing.T, tag, version, commit, content string) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "services"), 0o755))
	manifest := fmt.Sprintf(`kind: %s
schema: %s
id: %s
version: %s
minimum-codefly-version: ">=0.1.0"
artifact-roots:
  - services
contracts:
  composition: ">=2.0 <3.0"
`, corecomposition.PackageKind, corecomposition.PackageSchema, testPackageID, version)
	require.NoError(t, os.WriteFile(filepath.Join(root, corecomposition.PackageManifestFileName), []byte(manifest), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "services", "frontend.txt"), []byte(content), 0o644))

	archive, digest, err := corecomposition.CanonicalArchive(root)
	require.NoError(t, err)
	provenance := &corecomposition.Provenance{
		Schema: corecomposition.ProvenanceSchema, Package: testPackageID, Version: version,
		Repository: testRepositoryURL(), Ref: tag, Commit: commit,
		ArtifactMediaType: corecomposition.ArtifactMediaType, ArtifactDigest: digest,
		SignatureIdentity: testSigner,
	}
	provenanceData, err := json.Marshal(provenance)
	require.NoError(t, err)
	signature := ed25519.Sign(fixture.privateKey, provenanceData)
	fixture.putRelease(tag, commit, archive, provenanceData, signature)
}

// putRelease (re)registers tag's assets under fresh IDs, so re-tagging a
// release (a moved tag / re-signed provenance) serves distinct content from
// whatever a prior resolution already cached.
func (fixture *pinnedFixture) putRelease(tag, commit string, archive, provenance, signature []byte) {
	archiveID, provenanceID, signatureID := fixture.nextAssetID+1, fixture.nextAssetID+2, fixture.nextAssetID+3
	fixture.nextAssetID += 3
	fixture.assetsByID[archiveID] = archive
	fixture.assetsByID[provenanceID] = provenance
	fixture.assetsByID[signatureID] = signature
	fixture.releases[tag] = releaseAssets{
		commit:    commit,
		archiveID: archiveID, provenanceID: provenanceID, signatureID: signatureID,
		archiveSize: len(archive), provenanceSize: len(provenance), signatureSize: len(signature),
	}
}

func (fixture *pinnedFixture) signerKeyBase64() string {
	return base64.StdEncoding.EncodeToString(fixture.publicKey)
}

func (fixture *pinnedFixture) handle(writer http.ResponseWriter, request *http.Request) {
	fixture.requests.Add(1)
	path := strings.TrimPrefix(request.URL.Path, "/api/v3")
	prefix := fmt.Sprintf("/repos/%s/%s", testOwner, testRepoName)

	switch {
	case path == prefix+"/releases":
		writer.Header().Set("Content-Type", "application/json")
		var entries []string
		for tag := range fixture.releases {
			entries = append(entries, fmt.Sprintf(
				`{"tag_name":%q,"assets":[{"name":"module.tar"},{"name":"provenance.json"},{"name":"provenance.sig"}]}`, tag))
		}
		for _, tag := range fixture.plainTags {
			entries = append(entries, fmt.Sprintf(`{"tag_name":%q,"assets":[]}`, tag))
		}
		fmt.Fprintf(writer, "[%s]", strings.Join(entries, ","))
	case strings.HasPrefix(path, prefix+"/releases/tags/"):
		tag := strings.TrimPrefix(path, prefix+"/releases/tags/")
		assets, ok := fixture.releases[tag]
		if !ok {
			http.NotFound(writer, request)
			return
		}
		immutable := true
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{"id":1,"tag_name":%q,"draft":false,"immutable":%t,"assets":[`+
			`{"id":%d,"name":"module.tar","size":%d},`+
			`{"id":%d,"name":"provenance.json","size":%d},`+
			`{"id":%d,"name":"provenance.sig","size":%d}]}`,
			tag, immutable,
			assets.archiveID, assets.archiveSize, assets.provenanceID, assets.provenanceSize, assets.signatureID, assets.signatureSize)
	case strings.HasPrefix(path, prefix+"/git/ref/tags/"):
		tag := strings.TrimPrefix(path, prefix+"/git/ref/tags/")
		assets, ok := fixture.releases[tag]
		if !ok {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{"ref":"refs/tags/%s","object":{"type":"commit","sha":%q}}`, tag, assets.commit)
	case strings.HasPrefix(path, prefix+"/releases/assets/"):
		var id int64
		if _, err := fmt.Sscanf(strings.TrimPrefix(path, prefix+"/releases/assets/"), "%d", &id); err != nil {
			http.NotFound(writer, request)
			return
		}
		content, ok := fixture.assetsByID[id]
		if !ok {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/octet-stream")
		_, _ = writer.Write(content)
	default:
		http.NotFound(writer, request)
	}
}

// writeWorkspace writes a minimal workspace.codefly.yaml declaring
// module-trust for fixture's package/repository, so
// LoadModuleTrust/ResolvePinnedModule can side-parse it.
func writeWorkspace(t *testing.T, dir string, fixture *pinnedFixture) {
	t.Helper()
	doc := fmt.Sprintf(`name: wiki
layout: modules
module-trust:
  repositories:
    %s: %s
  signers:
    %s: %q
`, testPackageID, testRepositoryURL(), testSigner, fixture.signerKeyBase64())
	require.NoError(t, os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte(doc), 0o644))
}

func useFixtureGitHub(t *testing.T, fixture *pinnedFixture) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_API_URL", fixture.server.URL+"/api/v3/")
}

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

// allowTempDirCleanup restores write permission on every directory under root
// once the test finishes, so t.TempDir()'s own cleanup can delete a module
// cache tree the Materializer left read-only.
func allowTempDirCleanup(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
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
	fixture.addRelease(t, "0.1.0")
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
`, testPackageID, testRepositoryURL(), testSigner, fixture.signerKeyBase64())
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDir, resources.WorkspaceConfigurationName), []byte(doc), 0o644))
	ref := &resources.ModuleReference{Name: "saas", Source: testOwner + "/" + testRepoName, Version: "0.1.0"}

	resolved, err := ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(resolved.Dir, corecomposition.PackageManifestFileName))
}

func TestResolvePinnedModuleVerifiesAndMaterializes(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	fixture := newPinnedFixture(t)
	fixture.addRelease(t, "0.1.0")
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

	requestsAfterFirst := fixture.requests.Load()
	resolved2, err := ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.NoError(t, err)
	require.Equal(t, resolved.Dir, resolved2.Dir)
	require.Equal(t, requestsAfterFirst, fixture.requests.Load(), "a cached exact version must not hit the network again")
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
	fixture.addRelease(t, "0.1.0")
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
	requestsAfterFirst := fixture.requests.Load()
	_, err = ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.ErrorContains(t, err, "module subpath")
	require.Equal(t, requestsAfterFirst, fixture.requests.Load(), "the cached, already-verified package must not be re-fetched")
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
	fixture.addRelease(t, "0.1.0")
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

	requestsBeforeSecond := fixture.requests.Load()
	resolved2, err := ResolvePinnedModule(context.Background(), workspaceDir, ref)
	require.NoError(t, err)
	require.Greater(t, fixture.requests.Load(), requestsBeforeSecond,
		"a tampered cache entry must not be trusted without a network re-verification")

	content, err := os.ReadFile(filepath.Join(resolved2.Dir, "services", "frontend.txt"))
	require.NoError(t, err)
	require.Equal(t, "frontend", string(content), "the tampered file must be restored to the verified content")
}

func TestResolvePinnedModuleRejectsUntrustedSigner(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	fixture := newPinnedFixture(t)
	fixture.addRelease(t, "0.1.0")
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
	fixture.addRelease(t, "0.1.0")
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
	fixture.addReleaseAt(t, "0.1.0", strings.Repeat("b", 40), "retagged")
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
	fixture.addRelease(t, "0.1.0")
	fixture.addRelease(t, "0.2.0")
	fixture.addPlainRelease("v0.5.0")
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
	fixture.addPrefixedRelease(t, "0.3.0", testCommit, "prefixed")
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
	fixture.addRelease(t, "0.1.0")
	fixture.addPrefixedRelease(t, "0.4.0", testCommit, "prefixed")
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
	fixture.addRelease(t, "0.1.0")
	useFixtureGitHub(t, fixture)

	firstWorkspace := t.TempDir()
	allowTempDirCleanup(t, firstWorkspace)
	writeWorkspace(t, firstWorkspace, fixture)
	ref := &resources.ModuleReference{Name: "saas", Source: testOwner + "/" + testRepoName, Version: "0.1.0"}
	_, err := ResolvePinnedModule(context.Background(), firstWorkspace, ref)
	require.NoError(t, err)

	// The remote re-tags v0.1.0 at a different, internally self-consistent
	// commit and content.
	fixture.addReleaseAt(t, "0.1.0", strings.Repeat("b", 40), "retagged")

	secondWorkspace := t.TempDir()
	allowTempDirCleanup(t, secondWorkspace)
	writeWorkspace(t, secondWorkspace, fixture)
	_, err = ResolvePinnedModule(context.Background(), secondWorkspace, ref)
	require.Error(t, err)
	require.ErrorIs(t, err, corecomposition.ErrMovedTag)
}
