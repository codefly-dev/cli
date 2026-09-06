package composition

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/cli/pkg/gh"
	corecomposition "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/google/go-github/v89/github"
	"gopkg.in/yaml.v3"
)

// cacheMarkerFileName is the marker composition.Materializer writes into every
// materialized directory (core composition/materialize.go's cacheMarkerName,
// unexported there). It must be excluded when recomputing a cached tree's
// canonical digest, exactly as core's own verifyCache does internally.
const cacheMarkerFileName = ".codefly-cache.json"

// resolvedIndexName records, globally (not per workspace — see
// resolvedIndexRoot), the digest and peeled commit a (package, exact version)
// pair last verified to. It lets a concrete pin re-resolve to its cached,
// already-verified directory without a network round trip, and lets a moved
// tag be caught even the first time a *different* workspace or worktree
// resolves that same package version.
const resolvedIndexName = ".codefly-resolved-versions.json"

// resolvedEntry is one entry of the on-disk resolved-version index.
type resolvedEntry struct {
	Digest string `json:"digest"`
	Commit string `json:"commit"`
}

// PinnedResolution is what a pinned module resolved to: the materialized
// module directory plus the facts a `module.codefly.lock`-shaped overlay
// record (and `codefly doctor workspace`) can report.
type PinnedResolution struct {
	Dir     string
	Package string
	Version string
	Digest  string
	Commit  string
}

// ResolvePinnedModule resolves ref (a `source@version` module reference) to a
// verified, materialized module directory: it identifies the trusted GitHub
// package the reference names, fetches (or reuses a cached) module-package
// release, verifies its signature and digest against the workspace's
// `module-trust` policy, and extracts it into the workspace's
// content-addressed module cache. It fails closed — any resolution, network,
// or verification error is returned rather than falling back silently.
func ResolvePinnedModule(ctx context.Context, workspaceDir string, ref *resources.ModuleReference) (*PinnedResolution, error) {
	trust, packageID, pkg, err := loadTrustAndIdentity(workspaceDir, ref)
	if err != nil {
		return nil, err
	}
	materializer := corecomposition.NewMaterializer(workspaceDir)
	indexRoot := resolvedIndexRoot()

	if exactVersion, ok := exactPinnedVersion(ref.Version); ok {
		if entry, ok := readResolvedIndex(indexRoot, packageID, exactVersion); ok {
			if dir, cacheErr := cachedPackageDir(materializer, packageID, exactVersion, entry.Digest); cacheErr == nil {
				moduleDir, moduleErr := moduleSubdir(dir, ref.Module)
				if moduleErr != nil {
					return nil, moduleErr
				}
				return &PinnedResolution{
					Dir: moduleDir, Package: packageID, Version: exactVersion,
					Digest: entry.Digest, Commit: entry.Commit,
				}, nil
			}
		}
	}

	client, err := gh.NewClient()
	if err != nil {
		return nil, fmt.Errorf("configure GitHub client: %w", err)
	}
	version, err := resolvePackageVersion(ctx, client, pkg.Owner, pkg.RepositoryName, pkg.ArtifactAsset, ref.Version)
	if err != nil {
		return nil, err
	}
	resolver := corecomposition.NewGitHubResolver(client, nil, map[string]corecomposition.GitHubPackage{packageID: pkg})
	release, err := fetchPackageRelease(ctx, resolver, packageID, &pkg, version)
	if err != nil {
		return nil, err
	}
	verified, err := corecomposition.VerifyRelease(release, packageID, version, trust)
	if err != nil {
		return nil, err
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(release.Artifact))
	if previous, ok := readResolvedIndex(indexRoot, packageID, version); ok &&
		(previous.Digest != digest || previous.Commit != release.Commit) {
		return nil, fmt.Errorf("%w: %s@%s previously resolved to commit %s, now %s", corecomposition.ErrMovedTag, packageID, version, previous.Commit, release.Commit)
	}
	cacheDir, err := materializer.Materialize(ctx, verified)
	if err != nil {
		return nil, err
	}
	// Record the verified digest/commit before checking ref.Module: the
	// package itself verified and materialized successfully regardless of
	// whether this particular reference's module subpath exists in it, so a
	// second call (even with the same bad ref.Module) should hit the
	// no-network cache path rather than re-fetching from GitHub every time.
	if indexErr := writeResolvedIndex(ctx, indexRoot, packageID, version, resolvedEntry{Digest: digest, Commit: release.Commit}); indexErr != nil {
		return nil, indexErr
	}
	moduleDir, err := moduleSubdir(cacheDir, ref.Module)
	if err != nil {
		return nil, err
	}
	return &PinnedResolution{
		Dir: moduleDir, Package: packageID, Version: version,
		Digest: digest, Commit: release.Commit,
	}, nil
}

// CheckModuleTrustCoverage reports, without any network access, whether ref's
// pinned module resolves to a trusted package identity: a workspace with no
// `module-trust` at all, or one whose `module-trust` doesn't cover ref's
// repository, both error exactly as ResolvePinnedModule would fail later —
// letting `codefly doctor workspace` catch either case before a run is
// attempted, for every pinned module individually rather than only checking
// that *some* module-trust block exists.
func CheckModuleTrustCoverage(workspaceDir string, ref *resources.ModuleReference) error {
	_, _, _, err := loadTrustAndIdentity(workspaceDir, ref)
	return err
}

// loadTrustAndIdentity loads the workspace's module-trust policy and resolves
// ref's trusted GitHub package identity against it — the shared, network-free
// prefix of ResolvePinnedModule and CheckModuleTrustCoverage.
func loadTrustAndIdentity(workspaceDir string, ref *resources.ModuleReference) (corecomposition.TrustPolicy, string, corecomposition.GitHubPackage, error) {
	trust, packageOverrides, err := LoadModuleTrust(workspaceDir)
	if err != nil {
		return corecomposition.TrustPolicy{}, "", corecomposition.GitHubPackage{}, err
	}
	if trust == nil {
		return corecomposition.TrustPolicy{}, "", corecomposition.GitHubPackage{}, fmt.Errorf("module %s is pinned but workspace declares no module-trust; add the package's repository and signer, or set resolve.%s.git: true in codefly.local.yaml to use the unverified git clone", ref.Name, ref.Name)
	}
	packageID, pkg, err := resolvePackageIdentity(ref, *trust, packageOverrides[ref.Name])
	if err != nil {
		return corecomposition.TrustPolicy{}, "", corecomposition.GitHubPackage{}, err
	}
	return *trust, packageID, pkg, nil
}

// resolvedIndexRoot is deliberately global (not workspace-scoped, unlike the
// materializer's own cache) so a moved tag already caught by one workspace or
// git worktree is remembered when a *different* one resolves the same
// package version for the first time, instead of trusting it fresh.
func resolvedIndexRoot() string {
	return filepath.Join(resources.CodeflyHomeDir(), "modules")
}

// moduleSubdir joins module onto dir and confirms the result is populated,
// mirroring the git-clone fallback's check: a materialized package whose
// `module:` field names a subpath that does not exist is a configuration
// error to surface immediately, not a transient cache miss a network retry
// would fix.
func moduleSubdir(dir, module string) (string, error) {
	target := dir
	if module != "" {
		target = filepath.Join(dir, filepath.FromSlash(module))
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) == 0 {
		return "", fmt.Errorf("materialized module package at %s but module subpath %q is missing", dir, module)
	}
	return target, nil
}

func exactPinnedVersion(version string) (string, bool) {
	version = strings.TrimSpace(version)
	if version == "" || version == "latest" {
		return "", false
	}
	parsed, err := semver.NewVersion(strings.TrimPrefix(version, "v"))
	if err != nil {
		return "", false
	}
	return parsed.String(), true
}

// cachedPackageDir returns digest's cache directory for a no-network fast
// path, but only after confirming both its manifest identity and its
// canonical tree digest still match what was recorded when it was verified —
// a manifest-only check would let a directory tampered with (or corrupted)
// after materialization be trusted silently forever, defeating the point of
// verifying it in the first place.
func cachedPackageDir(materializer *corecomposition.Materializer, packageID, version, digest string) (string, error) {
	path, err := materializer.CachePath(digest)
	if err != nil {
		return "", err
	}
	manifest, err := corecomposition.LoadPackageManifest(path)
	if err != nil {
		return "", err
	}
	if manifest.ID != packageID || manifest.Version != version {
		return "", fmt.Errorf("cached module package at %s identifies %s@%s, want %s@%s", path, manifest.ID, manifest.Version, packageID, version)
	}
	if err := verifyCachedTreeDigest(path, digest); err != nil {
		return "", err
	}
	return path, nil
}

// verifyCachedTreeDigest recomputes path's canonical archive digest (see
// core composition.CanonicalArchive) excluding the cache marker file, the
// same computation core's Materializer performs internally on every cache
// hit, and rejects a mismatch. CanonicalArchive itself has no exclusion knob,
// so the tree is copied minus the marker into a scratch directory first.
func verifyCachedTreeDigest(path, expectedDigest string) error {
	scratch, err := os.MkdirTemp("", "codefly-verify-cache-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	if copyErr := copyTreeExcluding(path, scratch, cacheMarkerFileName); copyErr != nil {
		return copyErr
	}
	_, digest, err := corecomposition.CanonicalArchive(scratch)
	if err != nil {
		return err
	}
	if digest != expectedDigest {
		return fmt.Errorf("%w: cached module package at %s has digest %s, want %s", corecomposition.ErrDigestMismatch, path, digest, expectedDigest)
	}
	return nil
}

// copyTreeExcluding copies src into dst, skipping excludeName. Both reads and
// writes go through an os.Root scoped to src/dst respectively, rather than
// plain os.ReadFile/os.WriteFile on a filepath.Join'd path, so a symlink
// swapped in between WalkDir's directory listing and the read cannot redirect
// either side outside its own tree.
func copyTreeExcluding(src, dst, excludeName string) error {
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { _ = srcRoot.Close() }()
	if mkdirErr := os.MkdirAll(dst, 0o755); mkdirErr != nil {
		return mkdirErr
	}
	dstRoot, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer func() { _ = dstRoot.Close() }()
	return filepath.WalkDir(src, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, relErr := filepath.Rel(src, current)
		if relErr != nil {
			return relErr
		}
		if relative == "." {
			return nil
		}
		if relative == excludeName {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return dstRoot.MkdirAll(relative, 0o755)
		}
		info, err := srcRoot.Lstat(relative)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("cached module package tree contains a symlink at %s", relative)
		}
		data, err := srcRoot.ReadFile(relative)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		return dstRoot.WriteFile(relative, data, mode)
	})
}

func readResolvedIndex(root, packageID, version string) (resolvedEntry, bool) {
	data, err := os.ReadFile(filepath.Join(root, resolvedIndexName))
	if err != nil {
		return resolvedEntry{}, false
	}
	var index map[string]resolvedEntry
	if err := json.Unmarshal(data, &index); err != nil {
		return resolvedEntry{}, false
	}
	entry, ok := index[packageID+"@"+version]
	return entry, ok
}

// writeResolvedIndex serializes its read-modify-write of the shared index
// file with a cross-process lock and writes atomically: two concurrent
// resolutions (different packages, different `codefly run` processes) each
// doing a naive read-whole-map/set-one-key/write-whole-map cycle would
// otherwise silently lose whichever one wrote last, and a crash mid-write
// would otherwise leave the file truncated — either failure mode silently
// disables moved-tag detection for the lost entries instead of failing loud.
func writeResolvedIndex(ctx context.Context, root, packageID, version string, entry resolvedEntry) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	path := filepath.Join(root, resolvedIndexName)
	return WithFileLock(path+".lock", 30*time.Second, func() error {
		index := map[string]resolvedEntry{}
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &index)
		}
		index[packageID+"@"+version] = entry
		data, err := json.MarshalIndent(index, "", "  ")
		if err != nil {
			return err
		}
		return shared.WriteFileAtomic(ctx, path, data, 0o600)
	})
}

// resolvePackageIdentity maps ref's committed source to the module-trust
// package ID it corresponds to. A reference names a repository (via Source),
// not a package ID directly, so the ID is found by reverse lookup on
// `module-trust.repositories`; when several packages share a repository, the
// workspace must disambiguate with a `package:` field on the module (read
// from the raw workspace document, since core's ModuleReference does not
// carry it yet).
func resolvePackageIdentity(ref *resources.ModuleReference, trust corecomposition.TrustPolicy, packageOverride string) (string, corecomposition.GitHubPackage, error) {
	target := normalizeRepositoryURL(PinnedSourceURL(ref.Source))
	var matches []string
	for id, repository := range trust.Repositories {
		if normalizeRepositoryURL(repository) == target {
			matches = append(matches, id)
		}
	}
	var packageID string
	switch {
	case packageOverride != "":
		if !slices.Contains(matches, packageOverride) {
			return "", corecomposition.GitHubPackage{}, fmt.Errorf("module %s: package %q is not declared under module-trust.repositories for %s", ref.Name, packageOverride, target)
		}
		packageID = packageOverride
	case len(matches) == 1:
		packageID = matches[0]
	case len(matches) == 0:
		return "", corecomposition.GitHubPackage{}, fmt.Errorf("module %s: no module-trust.repositories entry matches %s; add its repository and signer to workspace.codefly.yaml", ref.Name, target)
	default:
		sort.Strings(matches)
		return "", corecomposition.GitHubPackage{}, fmt.Errorf("module %s: repository %s matches multiple trusted packages (%s); set package: on the module reference to disambiguate", ref.Name, target, strings.Join(matches, ", "))
	}
	owner, repo, err := splitGitHubRepository(trust.Repositories[packageID])
	if err != nil {
		return "", corecomposition.GitHubPackage{}, err
	}
	return packageID, corecomposition.GitHubPackage{
		// trust.Repositories[packageID] is already normalized by LoadModuleTrust,
		// so this matches provenance.Repository regardless of whether the user
		// wrote a trailing ".git" or "/" in workspace.codefly.yaml.
		Owner: owner, RepositoryName: repo, RepositoryURL: trust.Repositories[packageID],
		ArtifactAsset: "module.tar", ProvenanceAsset: "provenance.json", SignatureAsset: "provenance.sig",
	}, nil
}

// PinnedSourceURL turns a committed source identity into a clonable/parsable
// GitHub URL. A bare "owner/repo" slug (how `add module` records identity)
// becomes a GitHub HTTPS URL; a source that is already a URL is used verbatim.
func PinnedSourceURL(source string) string {
	if strings.Contains(source, "://") || strings.Contains(source, "@") {
		return source
	}
	return "https://github.com/" + source + ".git"
}

func normalizeRepositoryURL(raw string) string {
	return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(raw), "/"), ".git")
}

func splitGitHubRepository(repositoryURL string) (string, string, error) {
	parsed, err := url.Parse(normalizeRepositoryURL(repositoryURL))
	if err != nil || parsed.Host == "" {
		return "", "", fmt.Errorf("module-trust repository %q is not a valid GitHub URL", repositoryURL)
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("module-trust repository %q must be https://github.com/<owner>/<repo>", repositoryURL)
	}
	return parts[0], parts[1], nil
}

// modulePackageTagPrefix is a second, literal tag convention some producers
// publish module-package releases under (distinguishing the track from the
// deploy-counter releases sharing the plain "v*" namespace by tag name rather
// than by asset presence). GitHubResolver.Resolve can only ever fetch a plain
// "v"+version tag, so a release found only under this prefix is fetched via
// fetchPackageRelease's Fetch fallback instead.
const modulePackageTagPrefix = "module-package/v"

// resolvePackageVersion maps ref.Version to an exact module-package release
// version. An exact version is used as-is; "latest" or a semver constraint
// lists releases and picks the highest one satisfying it, recognizing a
// candidate under either tag convention a producer might publish: a literal
// "module-package/v"+version tag, or a plain "v"+version tag that also
// carries the module-package release assets (module.tar/provenance.json/
// provenance.sig) — a plain "v"+version tag without those assets is a
// deploy-counter release on the same tag namespace and is never a candidate.
func resolvePackageVersion(ctx context.Context, client *github.Client, owner, repo, artifactAsset, versionSpec string) (string, error) {
	if version, ok := exactPinnedVersion(versionSpec); ok {
		return version, nil
	}
	versionSpec = strings.TrimSpace(versionSpec)
	var constraint *semver.Constraints
	if versionSpec != "" && versionSpec != "latest" {
		var err error
		constraint, err = semver.NewConstraint(versionSpec)
		if err != nil {
			return "", fmt.Errorf("module package version %q is not a valid semantic version or constraint: %w", versionSpec, err)
		}
	}
	return highestPackageVersion(ctx, client, owner, repo, artifactAsset, constraint, versionSpec)
}

func highestPackageVersion(ctx context.Context, client *github.Client, owner, repo, artifactAsset string, constraint *semver.Constraints, label string) (string, error) {
	opts := &github.ListOptions{PerPage: 100}
	var versions []*semver.Version
	for {
		releases, resp, err := client.Repositories.ListReleases(ctx, owner, repo, opts)
		if err != nil {
			return "", fmt.Errorf("list releases of %s/%s: %w", owner, repo, err)
		}
		for _, release := range releases {
			tag := release.GetTagName()
			var versionText string
			switch {
			case strings.HasPrefix(tag, modulePackageTagPrefix):
				versionText = strings.TrimPrefix(tag, modulePackageTagPrefix)
			case strings.HasPrefix(tag, "v") && hasReleaseAsset(release.Assets, artifactAsset):
				versionText = strings.TrimPrefix(tag, "v")
			default:
				continue
			}
			version, err := semver.NewVersion(versionText)
			if err != nil {
				continue
			}
			if constraint != nil && !constraint.Check(version) {
				continue
			}
			versions = append(versions, version)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if len(versions) == 0 {
		if constraint != nil {
			return "", fmt.Errorf("no module-package release on %s/%s satisfies %q", owner, repo, label)
		}
		return "", fmt.Errorf("no module-package releases published on %s/%s", owner, repo)
	}
	sort.Sort(semver.Collection(versions))
	return versions[len(versions)-1].String(), nil
}

func hasReleaseAsset(assets []*github.ReleaseAsset, name string) bool {
	for _, asset := range assets {
		if asset.GetName() == name {
			return true
		}
	}
	return false
}

// fetchPackageRelease fetches version's release, trying both tag conventions
// GitHubResolver supports fetching: resolver.Resolve first (a plain
// "v"+version tag, the only one it can construct itself), then — because
// Resolve cannot address any other tag shape — resolver.Fetch with a
// structurally-valid placeholder Lock whose Source.Ref names the literal
// "module-package/v"+version tag verbatim (Fetch is the one exported method
// that takes an arbitrary ref rather than synthesizing one). Only Resolve's
// error is surfaced if both fail, since it names the more common convention.
func fetchPackageRelease(ctx context.Context, resolver *corecomposition.GitHubResolver, packageID string, pkg *corecomposition.GitHubPackage, version string) (*corecomposition.Release, error) {
	release, err := resolver.Resolve(ctx, corecomposition.ResolveRequest{Package: packageID, Version: version})
	if err == nil {
		return release, nil
	}
	prefixedTag := modulePackageTagPrefix + version
	fetched, fetchErr := resolver.Fetch(ctx, placeholderTagLock(packageID, pkg.RepositoryURL, version, prefixedTag))
	if fetchErr != nil {
		return nil, err
	}
	return fetched, nil
}

// placeholderTagLock builds a structurally-valid but otherwise-unused Lock
// purely to carry (repository, ref) through GitHubResolver.Fetch. Its
// Contracts/CompositionDigest/Commit fields exist only to satisfy
// Lock.Validate()'s format checks — Fetch never inspects their values, and
// VerifyRelease (not VerifyLockedRelease) is what actually authenticates the
// resulting release.
func placeholderTagLock(packageID, repositoryURL, version, ref string) *corecomposition.Lock {
	zeroDigest := "sha256:" + strings.Repeat("0", 64)
	return &corecomposition.Lock{
		Schema:  corecomposition.LockSchema,
		Module:  packageID,
		Package: packageID,
		Version: version,
		Source:  corecomposition.SourceLock{Repository: repositoryURL, Ref: ref, Commit: strings.Repeat("0", 40)},
		Artifact: corecomposition.ArtifactLock{
			MediaType: corecomposition.ArtifactMediaType,
			Digest:    zeroDigest,
			Signature: "placeholder",
		},
		Contracts:         map[string]string{corecomposition.ContractComposition: "1.0.0"},
		CompositionDigest: zeroDigest,
	}
}

// rawModuleTrust mirrors the `module-trust` block documented for
// workspace.codefly.yaml. It is side-parsed from the raw document rather than
// resources.Workspace because core's schema does not carry it yet (tracked
// separately as a small core change); this keeps the CLI-only until then.
type rawModuleTrust struct {
	Repositories map[string]string `yaml:"repositories"`
	Signers      map[string]string `yaml:"signers"`
}

type rawWorkspaceModuleTrustProbe struct {
	ModuleTrust *rawModuleTrust `yaml:"module-trust"`
	Modules     []struct {
		Name    string `yaml:"name"`
		Package string `yaml:"package"`
	} `yaml:"modules"`
}

// LoadModuleTrust side-parses workspace.codefly.yaml for its `module-trust`
// block and any per-module `package:` disambiguation. It returns (nil, _, nil)
// when the workspace declares no module-trust at all. Repository URLs are
// normalized (trailing "/" and ".git" stripped) so a workspace author's
// spelling of a repository never has to exactly match the byte-for-byte
// producer's `provenance.Repository` for VerifyRelease's strict comparison to
// succeed — both sides of that comparison flow from this same normalized map.
func LoadModuleTrust(workspaceDir string) (*corecomposition.TrustPolicy, map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(workspaceDir, resources.WorkspaceConfigurationName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read %s: %w", resources.WorkspaceConfigurationName, err)
	}
	var probe rawWorkspaceModuleTrustProbe
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", resources.WorkspaceConfigurationName, err)
	}
	overrides := map[string]string{}
	for _, module := range probe.Modules {
		if module.Package != "" {
			overrides[module.Name] = module.Package
		}
	}
	if probe.ModuleTrust == nil {
		return nil, overrides, nil
	}
	repositories := make(map[string]string, len(probe.ModuleTrust.Repositories))
	for id, repository := range probe.ModuleTrust.Repositories {
		repositories[id] = normalizeRepositoryURL(repository)
	}
	signers := map[string]ed25519.PublicKey{}
	for identity, encoded := range probe.ModuleTrust.Signers {
		key, err := decodeTrustSignerKey(encoded)
		if err != nil {
			return nil, nil, fmt.Errorf("module-trust signer %q: %w", identity, err)
		}
		signers[identity] = key
	}
	return &corecomposition.TrustPolicy{Repositories: repositories, Signers: signers}, overrides, nil
}

func decodeTrustSignerKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode base64 public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// NearestOverlayDir returns the directory of the codefly.local.yaml that
// resources.LoadLocalOverlay would load starting from start (searching
// upward, nearest first), or "" when none exists anywhere up the tree. Both
// `run`'s materialization and `doctor workspace`'s module-trust check must
// agree on this search so a directive in an ancestor overlay (the
// shared-monorepo layout) is honored identically by both.
func NearestOverlayDir(start string) string {
	current := start
	for {
		if _, err := os.Stat(filepath.Join(current, resources.LocalOverlayConfigurationName)); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

// GitFallbackOptOuts side-parses codefly.local.yaml at overlayDir for
// `resolve.<name>.git: true` directives. core's ModuleResolveDirective has no
// Git field, so a plain resources.LoadLocalOverlay silently drops it; reading
// the raw document here is what lets a workspace opt a module out of verified
// resolution back to the unverified git clone.
func GitFallbackOptOuts(overlayDir string) map[string]bool {
	data, err := os.ReadFile(filepath.Join(overlayDir, resources.LocalOverlayConfigurationName))
	if err != nil {
		return nil
	}
	var probe struct {
		Resolve map[string]struct {
			Git bool `yaml:"git"`
		} `yaml:"resolve"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil
	}
	optedOut := map[string]bool{}
	for name, entry := range probe.Resolve {
		if entry.Git {
			optedOut[name] = true
		}
	}
	return optedOut
}

// PreserveGitFallbackDirectives re-adds `git: true` to codefly.local.yaml
// entries in optedOut after a resources.SaveLocalOverlay write, which would
// otherwise silently drop it (ModuleResolveDirective has no Git field, so it
// never survives the typed marshal round trip). It edits the YAML document
// node-by-node so every other entry and key is left byte-for-byte as
// SaveLocalOverlay wrote it, and writes atomically so a crash mid-write
// cannot leave the overlay file truncated.
func PreserveGitFallbackDirectives(ctx context.Context, overlayDir string, optedOut map[string]bool) error {
	if len(optedOut) == 0 {
		return nil
	}
	path := filepath.Join(overlayDir, resources.LocalOverlayConfigurationName)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if unmarshalErr := yaml.Unmarshal(data, &doc); unmarshalErr != nil {
		return unmarshalErr
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	resolveNode := yamlMapValue(doc.Content[0], "resolve")
	if resolveNode == nil || resolveNode.Kind != yaml.MappingNode {
		return nil
	}
	changed := false
	for name := range optedOut {
		entry := yamlMapValue(resolveNode, name)
		if entry == nil || entry.Kind != yaml.MappingNode || yamlMapValue(entry, "git") != nil {
			continue
		}
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "git"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
		)
		changed = true
	}
	if !changed {
		return nil
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return shared.WriteFileAtomic(ctx, path, out, 0o600)
}

func yamlMapValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}
