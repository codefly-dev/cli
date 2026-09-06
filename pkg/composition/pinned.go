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

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/cli/pkg/gh"
	corecomposition "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/google/go-github/v89/github"
	"gopkg.in/yaml.v3"
)

// resolvedIndexName records, per workspace cache root, the digest a
// (package, exact version) pair last verified to. It lets a concrete pin
// re-resolve to its cached, already-verified directory without a network
// round trip, mirroring the git-clone fallback's offline-for-a-concrete-tag
// behavior.
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
	trust, packageOverrides, err := LoadModuleTrust(workspaceDir)
	if err != nil {
		return nil, err
	}
	if trust == nil {
		return nil, fmt.Errorf("module %s is pinned but workspace declares no module-trust; add the package's repository and signer, or set resolve.%s.git: true in codefly.local.yaml to use the unverified git clone", ref.Name, ref.Name)
	}
	packageID, pkg, err := resolvePackageIdentity(ref, *trust, packageOverrides[ref.Name])
	if err != nil {
		return nil, err
	}
	materializer := corecomposition.NewMaterializer(workspaceDir)

	if exactVersion, ok := exactPinnedVersion(ref.Version); ok {
		if entry, ok := readResolvedIndex(materializer.Root, packageID, exactVersion); ok {
			if dir, cacheErr := cachedPackageDir(materializer, packageID, exactVersion, entry.Digest); cacheErr == nil {
				return &PinnedResolution{
					Dir: joinModuleDir(dir, ref.Module), Package: packageID, Version: exactVersion,
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
	release, err := resolver.Resolve(ctx, corecomposition.ResolveRequest{Package: packageID, Version: version})
	if err != nil {
		return nil, err
	}
	verified, err := corecomposition.VerifyRelease(release, packageID, version, *trust)
	if err != nil {
		return nil, err
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(release.Artifact))
	if previous, ok := readResolvedIndex(materializer.Root, packageID, version); ok &&
		(previous.Digest != digest || previous.Commit != release.Commit) {
		return nil, fmt.Errorf("%w: %s@%s previously resolved to commit %s, now %s", corecomposition.ErrMovedTag, packageID, version, previous.Commit, release.Commit)
	}
	cacheDir, err := materializer.Materialize(ctx, verified)
	if err != nil {
		return nil, err
	}
	if err := writeResolvedIndex(materializer.Root, packageID, version, resolvedEntry{Digest: digest, Commit: release.Commit}); err != nil {
		return nil, err
	}
	return &PinnedResolution{
		Dir: joinModuleDir(cacheDir, ref.Module), Package: packageID, Version: version,
		Digest: digest, Commit: release.Commit,
	}, nil
}

func joinModuleDir(dir, module string) string {
	if module == "" {
		return dir
	}
	return filepath.Join(dir, filepath.FromSlash(module))
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
	return path, nil
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

func writeResolvedIndex(root, packageID, version string, entry resolvedEntry) error {
	path := filepath.Join(root, resolvedIndexName)
	index := map[string]resolvedEntry{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &index)
	}
	index[packageID+"@"+version] = entry
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
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

// resolvePackageVersion maps ref.Version to an exact module-package release
// version. An exact version is used as-is; "latest" or a semver constraint
// lists releases tagged module-package/v* and picks the highest one
// satisfying it. GitHubResolver.Resolve always fetches tag "v"+version, so a
// candidate release is one whose tag parses as a semver after that prefix;
// the module-package track is distinguished from the deploy-counter track
// (which shares the same "v*" tag namespace) by carrying the module-package
// release assets, not by any special tag naming — a release without the
// module.tar/provenance.json/provenance.sig set is a plain deploy tag and is
// never a candidate.
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
			if !hasReleaseAsset(release.Assets, artifactAsset) {
				continue
			}
			version, err := semver.NewVersion(strings.TrimPrefix(release.GetTagName(), "v"))
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
// when the workspace declares no module-trust at all.
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
	signers := map[string]ed25519.PublicKey{}
	for identity, encoded := range probe.ModuleTrust.Signers {
		key, err := decodeTrustSignerKey(encoded)
		if err != nil {
			return nil, nil, fmt.Errorf("module-trust signer %q: %w", identity, err)
		}
		signers[identity] = key
	}
	return &corecomposition.TrustPolicy{Repositories: probe.ModuleTrust.Repositories, Signers: signers}, overrides, nil
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
// SaveLocalOverlay wrote it.
func PreserveGitFallbackDirectives(overlayDir string, optedOut map[string]bool) error {
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
	return os.WriteFile(path, out, 0o600)
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
