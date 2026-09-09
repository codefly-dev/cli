package librarystore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/conformance/conformancetest"
	"github.com/stretchr/testify/require"
)

// requireNpm admits a test that needs the real npm CLI (npm pack / npm
// publish). A developer machine without Node steps aside; a CI job that
// declares the row required does not.
func requireNpm(t *testing.T) {
	t.Helper()
	conformancetest.Gate(t, "linux-amd64-native-npm", "npm")
}

func npmPackageDir(t *testing.T, name, version string) string {
	t.Helper()
	dir := t.TempDir()
	pkg := fmt.Sprintf(`{"name":%q,"version":%q,"main":"index.js"}`, name, version)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.js"), []byte("module.exports = {}\n"), 0o644))
	return dir
}

// fakeNpmRegistry is a real HTTP server implementing just enough of the npm
// registry protocol (GET packument, PUT publish) for the real npm CLI to pack
// and publish against it — not a mock of NpmStore.
type fakeNpmRegistry struct {
	mu       sync.Mutex
	docs     map[string]map[string]npmDist
	putCount int
	// hideNextVersionForGETs, when > 0, omits the version most recently
	// written by a PUT from that many subsequent GET responses before it
	// becomes visible — simulating a registry (GitHub Packages, notably) that
	// does not guarantee a packument GET reflects a publish immediately.
	hideNextVersionForGETs int
	hiddenVersion          string
}

func newFakeNpmRegistry() *fakeNpmRegistry {
	return &fakeNpmRegistry{docs: map[string]map[string]npmDist{}}
}

func (f *fakeNpmRegistry) seed(name, version string, dist npmDist) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.docs[name] == nil {
		f.docs[name] = map[string]npmDist{}
	}
	f.docs[name][version] = dist
}

func (f *fakeNpmRegistry) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			versions, ok := f.docs[name]
			doc := npmPackument{Name: name, Versions: map[string]npmVersionMeta{}}
			for v, dist := range versions {
				if f.hideNextVersionForGETs > 0 && v == f.hiddenVersion {
					continue
				}
				doc.Versions[v] = npmVersionMeta{Version: v, Dist: dist}
			}
			if f.hiddenVersion != "" && f.hideNextVersionForGETs > 0 {
				f.hideNextVersionForGETs--
				if f.hideNextVersionForGETs == 0 {
					f.hiddenVersion = ""
				}
			}
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"not found"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(doc)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				Versions map[string]struct {
					Dist npmDist `json:"dist"`
				} `json:"versions"`
			}
			_ = json.Unmarshal(body, &payload)
			f.mu.Lock()
			f.putCount++
			if f.docs[name] == nil {
				f.docs[name] = map[string]npmDist{}
			}
			for v, meta := range payload.Versions {
				f.docs[name][v] = meta.Dist
				if f.hideNextVersionForGETs > 0 {
					f.hiddenVersion = v
				}
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func newTestNpmStore(t *testing.T, registry *fakeNpmRegistry) *NpmStore {
	t.Helper()
	server := httptest.NewServer(registry.handler())
	t.Cleanup(server.Close)
	store := NewNpmStore(server.URL, "@codefly-dev")
	// No ambient NPM_TOKEN/NODE_AUTH_TOKEN/gh state: tests must not depend on
	// (or accidentally hit) the host's real npm credentials.
	store.token = func() string { return "test-token" }
	return store
}

func TestNpmStorePublishNewPackage(t *testing.T) {
	requireNpm(t)
	ctx := context.Background()
	registry := newFakeNpmRegistry()
	store := newTestNpmStore(t, registry)

	artifact := npmPackageDir(t, "@codefly-dev/authkit", "1.0.0")
	published, err := store.Publish(ctx, artifact, Coordinates{Language: LanguageTypeScript, Name: "authkit", Version: "1.0.0"})
	require.NoError(t, err)
	require.Equal(t, "@codefly-dev/authkit", published.ImportPath)
	require.Equal(t, "1.0.0", published.Version)
	require.Equal(t, 1, registry.putCount, "a new version must PUT exactly once")
	require.Equal(t, "npm install @codefly-dev/authkit@1.0.0", published.InstallHint)
	require.True(t, strings.HasPrefix(published.Digest, "sha512:"), "digest %q must carry the registry's integrity algorithm", published.Digest)
}

func TestNpmStorePublishIdempotentOnMatchingIntegrity(t *testing.T) {
	requireNpm(t)
	ctx := context.Background()
	registry := newFakeNpmRegistry()
	store := newTestNpmStore(t, registry)
	artifact := npmPackageDir(t, "@codefly-dev/authkit", "1.0.0")
	coords := Coordinates{Language: LanguageTypeScript, Name: "authkit", Version: "1.0.0"}

	first, err := store.Publish(ctx, artifact, coords)
	require.NoError(t, err)
	require.Equal(t, 1, registry.putCount)

	second, err := store.Publish(ctx, artifact, coords)
	require.NoError(t, err)
	require.Equal(t, 1, registry.putCount, "a re-run publishing byte-identical content must not PUT again")
	require.Equal(t, first.Digest, second.Digest)
	require.Equal(t, first.Ref, second.Ref)
}

func TestNpmStorePublishRejectsDifferentBytesAtSameVersion(t *testing.T) {
	requireNpm(t)
	ctx := context.Background()
	registry := newFakeNpmRegistry()
	registry.seed("@codefly-dev/authkit", "1.0.0", npmDist{Integrity: "sha512-doesnotmatch=="})
	store := newTestNpmStore(t, registry)

	artifact := npmPackageDir(t, "@codefly-dev/authkit", "1.0.0")
	_, err := store.Publish(ctx, artifact, Coordinates{Language: LanguageTypeScript, Name: "authkit", Version: "1.0.0"})
	require.ErrorContains(t, err, "different bytes")
	require.Equal(t, 0, registry.putCount, "a conflicting version must never be pushed to the registry")
}

// TestNpmStorePublishSurvivesTransientRegistryLagAfterPublish reproduces the
// GitHub-Packages-style eventual-consistency window: npm publish succeeds,
// but the very next packument GET (and the next few after it) still don't
// report the version. Publish must not fail — the write already happened and
// is irreversible — and must not panic on the nil packument a 404 produces
// mid-window; it must retry until the registry catches up.
func TestNpmStorePublishSurvivesTransientRegistryLagAfterPublish(t *testing.T) {
	requireNpm(t)
	ctx := context.Background()
	registry := newFakeNpmRegistry()
	registry.seed("@codefly-dev/authkit", "0.9.0", npmDist{Integrity: "sha512-old=="})
	registry.hideNextVersionForGETs = 3
	store := newTestNpmStore(t, registry)
	store.publishConfirmDelay = time.Millisecond

	artifact := npmPackageDir(t, "@codefly-dev/authkit", "1.0.0")
	published, err := store.Publish(ctx, artifact, Coordinates{Language: LanguageTypeScript, Name: "authkit", Version: "1.0.0"})
	require.NoError(t, err, "a publish that already succeeded at the registry must not be reported as a failure")
	require.Equal(t, 1, registry.putCount)
	require.Equal(t, "@codefly-dev/authkit", published.ImportPath)
	require.NotEmpty(t, published.Ref, "Ref must be populated even though the registry lagged behind the publish")
	require.True(t, strings.HasPrefix(published.Digest, "sha512:"))
}

// TestNpmStorePublishFallsBackWhenRegistryNeverConfirms covers the case where
// the registry never catches up within the retry budget: Publish must still
// report success (npm publish already committed the write) using its own
// conventional tarball URL, rather than erroring out or panicking on a nil
// packument.
func TestNpmStorePublishFallsBackWhenRegistryNeverConfirms(t *testing.T) {
	requireNpm(t)
	ctx := context.Background()
	registry := newFakeNpmRegistry()
	registry.hideNextVersionForGETs = 1000 // never resolves within the retry budget
	store := newTestNpmStore(t, registry)
	store.publishConfirmAttempts = 3
	store.publishConfirmDelay = time.Millisecond

	artifact := npmPackageDir(t, "@codefly-dev/authkit", "1.0.0")
	published, err := store.Publish(ctx, artifact, Coordinates{Language: LanguageTypeScript, Name: "authkit", Version: "1.0.0"})
	require.NoError(t, err)
	require.Equal(t, 1, registry.putCount)
	require.Equal(t, store.Registry+"/@codefly-dev/authkit/-/@codefly-dev/authkit-1.0.0.tgz", published.Ref)
}

func TestNpmStorePublishRejectsPackageNameOrVersionMismatch(t *testing.T) {
	ctx := context.Background()
	registry := newFakeNpmRegistry()
	store := newTestNpmStore(t, registry)

	wrongName := npmPackageDir(t, "@codefly-dev/other", "1.0.0")
	_, err := store.Publish(ctx, wrongName, Coordinates{Language: LanguageTypeScript, Name: "authkit", Version: "1.0.0"})
	require.ErrorContains(t, err, "@codefly-dev/authkit")

	wrongVersion := npmPackageDir(t, "@codefly-dev/authkit", "2.0.0")
	_, err = store.Publish(ctx, wrongVersion, Coordinates{Language: LanguageTypeScript, Name: "authkit", Version: "1.0.0"})
	require.ErrorContains(t, err, "does not match")
	require.Equal(t, 0, registry.putCount)
}

func TestNpmStoreResolveHighestSatisfyingVersion(t *testing.T) {
	registry := newFakeNpmRegistry()
	registry.seed("@codefly-dev/authkit", "1.0.0", npmDist{Integrity: "sha512-a==", Tarball: "http://x/authkit-1.0.0.tgz"})
	registry.seed("@codefly-dev/authkit", "1.2.0", npmDist{Integrity: "sha512-b==", Tarball: "http://x/authkit-1.2.0.tgz"})
	registry.seed("@codefly-dev/authkit", "2.0.0", npmDist{Integrity: "sha512-c==", Tarball: "http://x/authkit-2.0.0.tgz"})
	store := newTestNpmStore(t, registry)

	resolved, err := store.Resolve(context.Background(), LanguageTypeScript, "authkit", "^1.0.0")
	require.NoError(t, err)
	require.Equal(t, "1.2.0", resolved.Version)
	require.Equal(t, "http://x/authkit-1.2.0.tgz", resolved.Ref)
	require.Equal(t, "sha512:b==", resolved.Digest)
	require.Equal(t, "npm install @codefly-dev/authkit@1.2.0", resolved.InstallHint)
}

func TestNpmStoreResolveNoPublishedVersions(t *testing.T) {
	registry := newFakeNpmRegistry()
	store := newTestNpmStore(t, registry)
	_, err := store.Resolve(context.Background(), LanguageTypeScript, "authkit", "^1.0.0")
	require.ErrorContains(t, err, "no published versions")
}

func TestNpmStoreList(t *testing.T) {
	registry := newFakeNpmRegistry()
	registry.seed("@codefly-dev/authkit", "1.0.0", npmDist{Integrity: "sha512-a=="})
	registry.seed("@codefly-dev/authkit", "1.2.0", npmDist{Integrity: "sha512-b=="})
	store := newTestNpmStore(t, registry)

	versions, err := store.List(context.Background(), LanguageTypeScript, "authkit")
	require.NoError(t, err)
	require.Equal(t, []string{"1.2.0", "1.0.0"}, versions)
}

func TestNpmStorePublishWithoutCredentialFails(t *testing.T) {
	requireNpm(t)
	registry := newFakeNpmRegistry()
	server := httptest.NewServer(registry.handler())
	t.Cleanup(server.Close)
	store := NewNpmStore(server.URL, "@codefly-dev")
	store.token = func() string { return "" }

	artifact := npmPackageDir(t, "@codefly-dev/authkit", "1.0.0")
	_, err := store.Publish(context.Background(), artifact, Coordinates{Language: LanguageTypeScript, Name: "authkit", Version: "1.0.0"})
	require.ErrorContains(t, err, "no npm credential")
	require.Equal(t, 0, registry.putCount)
}

func TestNpmDigestConvertsIntegrityPrefix(t *testing.T) {
	require.Equal(t, "sha512:abcd==", npmDigest("sha512-abcd=="))
	require.Equal(t, "opaque", npmDigest("opaque"))
}

func TestRegistryHost(t *testing.T) {
	require.Equal(t, "npm.pkg.github.com", registryHost("https://npm.pkg.github.com"))
	require.Equal(t, "127.0.0.1:12345", registryHost("http://127.0.0.1:12345"))
}

func TestNpmTokenPrefersNpmTokenOverNodeAuthToken(t *testing.T) {
	t.Setenv("NPM_TOKEN", "from-npm-token")
	t.Setenv("NODE_AUTH_TOKEN", "from-node-auth-token")
	require.Equal(t, "from-npm-token", NpmToken("https://registry.npmjs.org"))
}

func TestNpmTokenFallsBackToNodeAuthToken(t *testing.T) {
	t.Setenv("NPM_TOKEN", "")
	t.Setenv("NODE_AUTH_TOKEN", "from-node-auth-token")
	require.Equal(t, "from-node-auth-token", NpmToken("https://registry.npmjs.org"))
}

func TestWriteNpmrcCreatesMappingWithoutALiteralSecret(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WriteNpmrc(dir, "@codefly-dev", "https://npm.pkg.github.com"))

	data, err := os.ReadFile(filepath.Join(dir, ".npmrc"))
	require.NoError(t, err)
	require.Contains(t, string(data), "@codefly-dev:registry=https://npm.pkg.github.com")
	require.Contains(t, string(data), "//npm.pkg.github.com/:_authToken=${NODE_AUTH_TOKEN}")
	require.NotContains(t, string(data), "s3cr3t", "the file must never carry a literal token")
}

func TestWriteNpmrcIsIdempotentAndPreservesExistingContent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".npmrc"), []byte("save-exact=true\n"), 0o644))

	require.NoError(t, WriteNpmrc(dir, "@codefly-dev", "https://npm.pkg.github.com"))
	require.NoError(t, WriteNpmrc(dir, "@codefly-dev", "https://npm.pkg.github.com"))

	data, err := os.ReadFile(filepath.Join(dir, ".npmrc"))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(data), "@codefly-dev:registry="), "a second call must not duplicate the mapping")
	require.Contains(t, string(data), "save-exact=true", "pre-existing, unrelated npmrc content must be preserved")
}

func TestWriteNpmrcRefusesToOverwriteAConflictingScopeMapping(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WriteNpmrc(dir, "@codefly-dev", "https://npm.pkg.github.com"))

	err := WriteNpmrc(dir, "@codefly-dev", "https://registry.example.com")
	require.ErrorContains(t, err, "already maps")
}
