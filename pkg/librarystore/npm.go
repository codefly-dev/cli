package librarystore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Masterminds/semver"
)

// NpmStore publishes each library export as a versioned npm package under
// Scope on Registry — a GitHub Packages feed (npm.pkg.github.com) by default,
// or any npm-compatible registry a workspace configures as a BYO feed.
//
// Publish shells out to the real npm CLI (`npm pack`, `npm publish`): npm
// owns tarball packing and the registry publish protocol, and reimplementing
// either would drift from what `npm install` actually consumes.
type NpmStore struct {
	Registry string
	Scope    string

	// token resolves the registry credential. Injectable so tests can exercise
	// the store without ambient NPM_TOKEN/gh state.
	token func() string
	// httpClient issues the packument GET. Injectable so tests point it at an
	// httptest server instead of the real registry.
	httpClient *http.Client
	// runNpm runs `npm <args...>` in dir with env appended to the ambient
	// environment. Tests do not override this: they run the real npm CLI
	// against an httptest registry, exercising the actual pack/publish
	// behavior rather than a mock of the store.
	runNpm func(ctx context.Context, dir string, env []string, args ...string) (string, error)
}

// NewNpmStore returns a store publishing scope-scoped packages to registry.
func NewNpmStore(registry, scope string) *NpmStore {
	registry = strings.TrimSuffix(registry, "/")
	s := &NpmStore{Registry: registry, Scope: scope, httpClient: http.DefaultClient}
	s.token = func() string { return npmToken(registry) }
	s.runNpm = runNpmCommand
	return s
}

// npmToken resolves the registry credential from NPM_TOKEN, then
// NODE_AUTH_TOKEN, then — for a GitHub Packages registry only — the `gh` CLI's
// stored credential, mirroring githubToken's fallback shape for the GitHub
// case.
func npmToken(registry string) string {
	if t := strings.TrimSpace(os.Getenv("NPM_TOKEN")); t != "" {
		return t
	}
	if t := strings.TrimSpace(os.Getenv("NODE_AUTH_TOKEN")); t != "" {
		return t
	}
	if registryHost(registry) != "npm.pkg.github.com" {
		return ""
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func registryHost(registry string) string {
	host := strings.TrimPrefix(strings.TrimPrefix(registry, "https://"), "http://")
	if index := strings.Index(host, "/"); index >= 0 {
		host = host[:index]
	}
	return host
}

type npmPackageJSON struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func readNpmPackageJSON(dir string) (npmPackageJSON, error) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return npmPackageJSON{}, fmt.Errorf("librarystore: a TypeScript library export must contain a package.json: %w", err)
	}
	var pkg npmPackageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return npmPackageJSON{}, fmt.Errorf("librarystore: invalid package.json: %w", err)
	}
	return pkg, nil
}

type npmDist struct {
	Integrity string `json:"integrity"`
	Tarball   string `json:"tarball"`
}

type npmVersionMeta struct {
	Version string  `json:"version"`
	Dist    npmDist `json:"dist"`
}

// npmPackument is the subset of the npm registry's package document this
// store reads: https://github.com/npm/registry/blob/master/docs/responses/package-metadata.md
type npmPackument struct {
	Name     string                    `json:"name"`
	Versions map[string]npmVersionMeta `json:"versions"`
}

func (s *NpmStore) packageName(name string) string {
	return s.Scope + "/" + name
}

// packageURL is the registry document URL for a scoped package: the scope's
// leading "@" is sent verbatim, but the "/" separating scope from name must
// be percent-encoded or the registry parses it as a path segment.
func (s *NpmStore) packageURL(name string) string {
	return s.Registry + "/" + s.Scope + "%2F" + name
}

// fetchPackument returns nil (not an error) when the package does not exist
// yet — the caller decides whether that means "nothing to compare against"
// (Publish) or "nothing to resolve" (Resolve/List).
func (s *NpmStore) fetchPackument(ctx context.Context, name string) (*npmPackument, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.packageURL(name), nil)
	if err != nil {
		return nil, err
	}
	if token := s.token(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("librarystore: fetch %s packument: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("librarystore: fetch %s packument: %s: %s", name, resp.Status, strings.TrimSpace(string(body)))
	}
	var doc npmPackument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("librarystore: decode %s packument: %w", name, err)
	}
	return &doc, nil
}

func (s *NpmStore) Publish(ctx context.Context, artifactDir string, c Coordinates) (Published, error) {
	if c.Language != LanguageTypeScript {
		return Published{}, fmt.Errorf("librarystore: npm store only publishes %s libraries, got %s", LanguageTypeScript, c.Language)
	}
	version, err := semver.NewVersion(strings.TrimPrefix(c.Version, "v"))
	if err != nil {
		return Published{}, fmt.Errorf("librarystore: %q is not a semantic version: %w", c.Version, err)
	}
	c.Version = version.String()
	name := s.packageName(c.Name)

	pkg, err := readNpmPackageJSON(artifactDir)
	if err != nil {
		return Published{}, err
	}
	if pkg.Name != name {
		return Published{}, fmt.Errorf("librarystore: package.json declares %q but consumers will require %q", pkg.Name, name)
	}
	if pkg.Version != c.Version {
		return Published{}, fmt.Errorf("librarystore: package.json version %q does not match %q", pkg.Version, c.Version)
	}

	work, err := os.MkdirTemp("", "codefly-library-publish-*")
	if err != nil {
		return Published{}, err
	}
	defer os.RemoveAll(work)
	if err = replaceTrackedTree(work, artifactDir); err != nil {
		return Published{}, fmt.Errorf("stage artifact: %w", err)
	}

	integrity, err := s.pack(ctx, work)
	if err != nil {
		return Published{}, err
	}

	packument, err := s.fetchPackument(ctx, c.Name)
	if err != nil {
		return Published{}, err
	}
	if packument != nil {
		if existing, ok := packument.Versions[c.Version]; ok {
			if existing.Dist.Integrity == integrity {
				return s.published(c, name, existing.Dist.Tarball, integrity), nil
			}
			return Published{}, fmt.Errorf("librarystore: version %s already published with different bytes; bump the version", c.Version)
		}
	}
	if err = s.publishToRegistry(ctx, work); err != nil {
		return Published{}, err
	}
	published, err := s.fetchPackument(ctx, c.Name)
	if err != nil {
		return Published{}, err
	}
	dist, ok := published.Versions[c.Version]
	if !ok {
		return Published{}, fmt.Errorf("librarystore: %s %s published but the registry does not report it yet", c.Name, c.Version)
	}
	return s.published(c, name, dist.Dist.Tarball, integrity), nil
}

// pack runs `npm pack --json` in dir, which builds the publish tarball from
// package.json + files without touching the registry, and returns its
// content integrity. The tarball is written outside dir (--pack-destination):
// npm pack does not exclude its own output by default, so packing into dir
// itself would fold the previous run's tarball into the next one — including
// the one npm publish creates right after this call — and corrupt the
// integrity comparison Publish relies on for idempotency.
func (s *NpmStore) pack(ctx context.Context, dir string) (integrity string, err error) {
	dest, err := os.MkdirTemp("", "codefly-library-pack-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dest)
	out, err := s.runNpm(ctx, dir, nil, "pack", "--json", "--pack-destination", dest)
	if err != nil {
		return "", fmt.Errorf("librarystore: npm pack: %w", err)
	}
	var results []struct {
		Integrity string `json:"integrity"`
	}
	if err = json.Unmarshal([]byte(out), &results); err != nil || len(results) == 0 {
		return "", fmt.Errorf("librarystore: parse npm pack output: %w", err)
	}
	if results[0].Integrity == "" {
		return "", fmt.Errorf("librarystore: npm pack did not report a tarball integrity (npm >= 8 required)")
	}
	return results[0].Integrity, nil
}

// publishToRegistry writes a project-local .npmrc scoping Registry to Scope
// and authenticating it, then runs `npm publish`. The token is passed both
// through .npmrc (for npm's own registry resolution) and NODE_AUTH_TOKEN (the
// variable npm's config conventionally interpolates `${NODE_AUTH_TOKEN}`
// from) so it matches how npm.pkg.github.com publishing is documented.
func (s *NpmStore) publishToRegistry(ctx context.Context, dir string) error {
	token := s.token()
	if token == "" {
		return fmt.Errorf("librarystore: no npm credential (set NPM_TOKEN, NODE_AUTH_TOKEN, or authenticate `gh`)")
	}
	npmrc := fmt.Sprintf("%s:registry=%s\n//%s/:_authToken=%s\n", s.Scope, s.Registry, registryHost(s.Registry), token)
	if err := os.WriteFile(filepath.Join(dir, ".npmrc"), []byte(npmrc), 0o600); err != nil {
		return fmt.Errorf("librarystore: write .npmrc: %w", err)
	}
	env := []string{"NODE_AUTH_TOKEN=" + token}
	if _, err := s.runNpm(ctx, dir, env, "publish", "--registry", s.Registry, "--access", "public"); err != nil {
		return fmt.Errorf("librarystore: npm publish: %w", err)
	}
	return nil
}

func (s *NpmStore) Resolve(ctx context.Context, language Language, name, constraint string) (Published, error) {
	if language != LanguageTypeScript {
		return Published{}, fmt.Errorf("librarystore: npm store only resolves %s libraries, got %s", LanguageTypeScript, language)
	}
	packument, err := s.fetchPackument(ctx, name)
	if err != nil {
		return Published{}, err
	}
	if packument == nil || len(packument.Versions) == 0 {
		return Published{}, fmt.Errorf("librarystore: no published versions of %s", name)
	}
	check, err := semver.NewConstraint(strings.TrimSpace(constraint))
	if err != nil {
		return Published{}, fmt.Errorf("librarystore: invalid version constraint %q: %w", constraint, err)
	}
	var best *semver.Version
	var bestMeta npmVersionMeta
	for raw, meta := range packument.Versions {
		v, err := semver.NewVersion(raw)
		if err != nil {
			continue
		}
		if check.Check(v) && (best == nil || v.GreaterThan(best)) {
			best, bestMeta = v, meta
		}
	}
	if best == nil {
		return Published{}, fmt.Errorf("librarystore: no published version of %s satisfies %q", name, constraint)
	}
	coordinates := Coordinates{Language: language, Name: name, Version: best.String()}
	return s.published(coordinates, s.packageName(name), bestMeta.Dist.Tarball, bestMeta.Dist.Integrity), nil
}

func (s *NpmStore) List(ctx context.Context, language Language, name string) ([]string, error) {
	if language != LanguageTypeScript {
		return nil, fmt.Errorf("librarystore: npm store only lists %s libraries, got %s", LanguageTypeScript, language)
	}
	packument, err := s.fetchPackument(ctx, name)
	if err != nil {
		return nil, err
	}
	if packument == nil {
		return nil, nil
	}
	versions := make([]*semver.Version, 0, len(packument.Versions))
	for raw := range packument.Versions {
		if v, err := semver.NewVersion(raw); err == nil {
			versions = append(versions, v)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].GreaterThan(versions[j]) })
	result := make([]string, len(versions))
	for i, v := range versions {
		result[i] = v.String()
	}
	return result, nil
}

func (s *NpmStore) published(c Coordinates, name, tarball, integrity string) Published {
	return Published{
		Coordinates: c,
		ImportPath:  name,
		Ref:         tarball,
		Location:    s.Registry,
		Digest:      npmDigest(integrity),
		InstallHint: fmt.Sprintf("npm install %s@%s", name, c.Version),
	}
}

// npmDigest converts an SRI integrity string ("<algorithm>-<base64>") to this
// store's "<algorithm>:<base64>" Digest convention.
func npmDigest(integrity string) string {
	algorithm, digest, ok := strings.Cut(integrity, "-")
	if !ok {
		return integrity
	}
	return algorithm + ":" + digest
}

func runNpmCommand(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	//nolint:gosec // npm is invoked with internal subcommands and store-controlled arguments, never a shell.
	cmd := exec.CommandContext(ctx, "npm", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("npm %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
