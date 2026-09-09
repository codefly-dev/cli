// Package pinnedfixture serves a real, signed module-package release over an
// httptest GitHub API double. It exists so both the resolver's own tests and
// the `run`-side materialization tests exercise the actual verification path —
// canonical archive, provenance, ed25519 signature, digest — instead of each
// standing up its own approximation of it.
package pinnedfixture

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	corecomposition "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// ModulePackageTagPrefix is the literal tag convention some producers publish
// module-package releases under, alongside the plain "v"+version one.
const ModulePackageTagPrefix = "module-package/v"

// AllowTempDirCleanup restores write permission on every directory under root
// once the test finishes, so t.TempDir()'s own cleanup can delete a module
// cache tree the Materializer left read-only.
func AllowTempDirCleanup(t *testing.T, root string) {
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

// Close stops serving, so a test can assert that a resolution which should be
// answerable from the local cache really needs no network at all.
func (fixture *Fixture) Close() {
	fixture.server.Close()
}

// Requests is how many HTTP requests the fixture has served, so a test can
// assert that a cached resolution touched the network zero times.
func (fixture *Fixture) Requests() int64 {
	return fixture.requests.Load()
}

const (
	Owner     = "codefly-dev"
	RepoName  = "module-saas-starter"
	PackageID = "codefly/saas-starter"
	Signer    = "https://github.com/codefly-dev/module-saas-starter/.github/workflows/release.yml@refs/heads/main"
	Commit    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func RepositoryURL() string {
	return fmt.Sprintf("https://github.com/%s/%s", Owner, RepoName)
}

// Fixture is a GitHub API + release-asset test double serving one
// signed module-package release per registered version, replicating core's
// composition_test.go httptest harness (fixtureResolver /
// TestGitHubResolverFetchesAssetsAndPeeledCommit) against the exported
// composition API this package builds on.
type Fixture struct {
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

func New(t *testing.T) *Fixture {
	t.Helper()
	seed := strings.Repeat("k", ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed([]byte(seed))
	fixture := &Fixture{
		privateKey: privateKey,
		publicKey:  privateKey.Public().(ed25519.PublicKey),
		releases:   map[string]releaseAssets{},
		assetsByID: map[int64][]byte{},
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.handle))
	t.Cleanup(fixture.server.Close)
	return fixture
}

// AddRelease registers a module-package release tagged v<version>, signed and
// digest-matched for immediate verification, at Commit.
func (fixture *Fixture) AddRelease(t *testing.T, version string) {
	t.Helper()
	fixture.AddReleaseAt(t, version, Commit, "frontend")
}

// AddPlainRelease registers a release at tag with no module-package assets —
// the deploy-counter track sharing the same "v*" tag namespace, which must
// never be picked as a candidate module-package version.
func (fixture *Fixture) AddPlainRelease(tag string) {
	fixture.plainTags = append(fixture.plainTags, tag)
}

// AddReleaseAt (re)registers tag "v"+version as a fresh, internally
// self-consistent release at commit — different content so a different commit
// also means a different artifact digest, mirroring what an actual re-tag
// would republish. Used to simulate the remote moving an immutable tag.
func (fixture *Fixture) AddReleaseAt(t *testing.T, version, commit, content string) {
	t.Helper()
	fixture.addReleaseTagged(t, "v"+version, version, commit, content)
}

// AddPrefixedRelease registers a release under the literal
// "module-package/v"+version tag some producers use instead of a plain
// "v"+version tag, exercising fetchPackageRelease's Fetch fallback (Resolve
// can only ever fetch a plain "v"+version tag).
func (fixture *Fixture) AddPrefixedRelease(t *testing.T, version, commit, content string) {
	t.Helper()
	fixture.addReleaseTagged(t, ModulePackageTagPrefix+version, version, commit, content)
}

func (fixture *Fixture) addReleaseTagged(t *testing.T, tag, version, commit, content string) {
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
`, corecomposition.PackageKind, corecomposition.PackageSchema, PackageID, version)
	require.NoError(t, os.WriteFile(filepath.Join(root, corecomposition.PackageManifestFileName), []byte(manifest), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "services", "frontend.txt"), []byte(content), 0o644))

	archive, digest, err := corecomposition.CanonicalArchive(root)
	require.NoError(t, err)
	provenance := &corecomposition.Provenance{
		Schema: corecomposition.ProvenanceSchema, Package: PackageID, Version: version,
		Repository: RepositoryURL(), Ref: tag, Commit: commit,
		ArtifactMediaType: corecomposition.ArtifactMediaType, ArtifactDigest: digest,
		SignatureIdentity: Signer,
	}
	provenanceData, err := json.Marshal(provenance)
	require.NoError(t, err)
	signature := ed25519.Sign(fixture.privateKey, provenanceData)
	fixture.putRelease(tag, commit, archive, provenanceData, signature)
}

// putRelease (re)registers tag's assets under fresh IDs, so re-tagging a
// release (a moved tag / re-signed provenance) serves distinct content from
// whatever a prior resolution already cached.
func (fixture *Fixture) putRelease(tag, commit string, archive, provenance, signature []byte) {
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

func (fixture *Fixture) SignerKeyBase64() string {
	return base64.StdEncoding.EncodeToString(fixture.publicKey)
}

func (fixture *Fixture) handle(writer http.ResponseWriter, request *http.Request) {
	fixture.requests.Add(1)
	path := strings.TrimPrefix(request.URL.Path, "/api/v3")
	prefix := fmt.Sprintf("/repos/%s/%s", Owner, RepoName)

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

// WriteWorkspace writes a minimal workspace.codefly.yaml declaring
// module-trust for fixture's package/repository, so
// LoadModuleTrust/ResolvePinnedModule can side-parse it.
func WriteWorkspace(t *testing.T, dir string, fixture *Fixture) {
	t.Helper()
	doc := fmt.Sprintf(`name: wiki
layout: modules
module-trust:
  repositories:
    %s: %s
  signers:
    %s: %q
`, PackageID, RepositoryURL(), Signer, fixture.SignerKeyBase64())
	require.NoError(t, os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte(doc), 0o644))
}

func (fixture *Fixture) UseGitHub(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_API_URL", fixture.server.URL+"/api/v3/")
}
