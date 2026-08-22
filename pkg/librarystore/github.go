package librarystore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Masterminds/semver"
)

// gitDir is the directory git owns in a working tree. It is never part of
// published content: skipped when staging a source tree and when digesting.
const gitDir = ".git"

// GitHubStore publishes each library export to its own GitHub repository, tagged
// with a semantic version. A Go export published this way is resolvable by
// `go get github.com/<owner>/<name>-go@vX.Y.Z` with no codefly toolchain, since
// a git repository with semver tags is a native Go module source.
type GitHubStore struct {
	// Owner is the GitHub organization or user the published repositories live in.
	Owner string
	// remoteFor resolves the git remote for a library export. Tests override it to
	// point at a local bare repository so publish/resolve exercise real git.
	remoteFor func(language Language, name string) string
	// commitIdentity is the author used for release commits.
	commitName  string
	commitEmail string
}

// NewGitHubStore returns a store publishing to repositories under owner.
func NewGitHubStore(owner string) *GitHubStore {
	s := &GitHubStore{Owner: owner, commitName: "codefly", commitEmail: "bot@codefly.dev"}
	s.remoteFor = func(language Language, name string) string {
		return fmt.Sprintf("https://github.com/%s/%s.git", owner, repositoryName(language, name))
	}
	return s
}

// repositoryName is the per-export repository name, e.g. "authkit-go".
func repositoryName(language Language, name string) string {
	return fmt.Sprintf("%s-%s", name, language)
}

// goModulePath derives the Go module path from a remote URL: the identity a
// consumer passes to `go get`. It is only meaningful for real remote URLs; a
// local test remote yields a path used purely for assertions.
func goModulePath(remote string) string {
	path := strings.TrimSuffix(remote, ".git")
	path = strings.TrimPrefix(path, "https://")
	path = strings.TrimPrefix(path, "git@")
	path = strings.Replace(path, ":", "/", 1)
	return path
}

func versionTag(version string) string {
	return "v" + strings.TrimPrefix(version, "v")
}

func (s *GitHubStore) Publish(ctx context.Context, artifactDir string, c Coordinates) (Published, error) {
	if c.Language != LanguageGo {
		return Published{}, fmt.Errorf("librarystore: publishing %s libraries is not implemented yet", c.Language)
	}
	version, err := semver.NewVersion(strings.TrimPrefix(c.Version, "v"))
	if err != nil {
		return Published{}, fmt.Errorf("librarystore: %q is not a semantic version: %w", c.Version, err)
	}
	c.Version = version.String()
	remote := s.remoteFor(c.Language, c.Name)
	importPath := goModulePath(remote)
	tag := versionTag(c.Version)

	// The product promise is that consumers run `go get <importPath>@<tag>`. That
	// only works when the published go.mod declares exactly that module path, so a
	// mismatch must fail here, at publish time — after the tag lands the version
	// is immutable and a broken release could never be corrected.
	if err = validateGoModulePath(artifactDir, importPath); err != nil {
		return Published{}, err
	}
	digest, err := treeDigest(artifactDir)
	if err != nil {
		return Published{}, err
	}

	work, err := os.MkdirTemp("", "codefly-library-publish-*")
	if err != nil {
		return Published{}, err
	}
	defer os.RemoveAll(work)

	if err = s.git(ctx, "", "clone", "--quiet", remote, work); err != nil {
		return Published{}, fmt.Errorf("clone %s (create the library repository first if it does not exist yet): %w", remote, err)
	}
	if s.tagExists(ctx, work, tag) {
		return Published{}, fmt.Errorf("librarystore: %s %s is already published (versions are immutable)", c.Name, tag)
	}
	branch, err := s.defaultBranch(ctx, work)
	if err != nil {
		return Published{}, err
	}
	if err = replaceTrackedTree(work, artifactDir); err != nil {
		return Published{}, fmt.Errorf("stage artifact: %w", err)
	}
	if err = s.git(ctx, work, "add", "-A"); err != nil {
		return Published{}, err
	}
	message := fmt.Sprintf("release %s %s", c.Name, tag)
	// --allow-empty: a release whose content is identical to the previous one
	// (a version bump with no code change) is a valid release, not an error.
	if err = s.git(ctx, work,
		"-c", "user.name="+s.commitName, "-c", "user.email="+s.commitEmail,
		"commit", "--quiet", "--allow-empty", "-m", message); err != nil {
		return Published{}, fmt.Errorf("commit release: %w", err)
	}
	if err = s.git(ctx, work,
		"-c", "user.name="+s.commitName, "-c", "user.email="+s.commitEmail,
		"tag", "-a", tag, "-m", message); err != nil {
		return Published{}, fmt.Errorf("tag release: %w", err)
	}
	if err = s.git(ctx, work, "push", "--quiet", "origin", branch); err != nil {
		return Published{}, fmt.Errorf("push branch: %w", err)
	}
	if err = s.git(ctx, work, "push", "--quiet", "origin", tag); err != nil {
		return Published{}, fmt.Errorf("push tag: %w", err)
	}
	commit, err := s.output(ctx, work, "rev-parse", tag+"^{commit}")
	if err != nil {
		return Published{}, err
	}
	return s.published(c, remote, strings.TrimSpace(commit), digest), nil
}

// validateGoModulePath requires the artifact's go.mod to declare importPath as
// its module path — the identity consumers will `go get`.
func validateGoModulePath(artifactDir, importPath string) error {
	data, err := os.ReadFile(filepath.Join(artifactDir, "go.mod"))
	if err != nil {
		return fmt.Errorf("librarystore: a Go library export must contain a go.mod: %w", err)
	}
	declared, err := parseGoModulePath(data)
	if err != nil {
		return fmt.Errorf("librarystore: invalid go.mod: %w", err)
	}
	if declared != importPath {
		return fmt.Errorf(
			"librarystore: go.mod declares module %q but consumers will require %q; the export must be rewritten to the published module path before publishing",
			declared,
			importPath,
		)
	}
	return nil
}

func parseGoModulePath(data []byte) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		if index := strings.Index(line, "//"); index >= 0 {
			line = line[:index]
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			return strings.Trim(fields[1], `"`), nil
		}
	}
	return "", fmt.Errorf("no module directive")
}

func (s *GitHubStore) Resolve(ctx context.Context, language Language, name, constraint string) (Published, error) {
	if language != LanguageGo {
		return Published{}, fmt.Errorf("librarystore: resolving %s libraries is not implemented yet", language)
	}
	remote := s.remoteFor(language, name)
	tagged, err := s.listTagged(ctx, remote)
	if err != nil {
		return Published{}, err
	}
	if len(tagged) == 0 {
		return Published{}, fmt.Errorf("librarystore: no published versions of %s", name)
	}
	check, err := semver.NewConstraint(strings.TrimSpace(constraint))
	if err != nil {
		return Published{}, fmt.Errorf("librarystore: invalid version constraint %q: %w", constraint, err)
	}
	var best *taggedVersion
	for index := range tagged {
		candidate := &tagged[index]
		if check.Check(candidate.version) && (best == nil || candidate.version.GreaterThan(best.version)) {
			best = candidate
		}
	}
	if best == nil {
		return Published{}, fmt.Errorf("librarystore: no published version of %s satisfies %q", name, constraint)
	}
	if !isCommitHash(best.ref) {
		return Published{}, fmt.Errorf("librarystore: tag %s of %s resolves to invalid commit %q", versionTag(best.version.String()), name, best.ref)
	}
	digest, err := s.digestAtTag(ctx, remote, versionTag(best.version.String()))
	if err != nil {
		return Published{}, err
	}
	coordinates := Coordinates{Language: language, Name: name, Version: best.version.String()}
	return s.published(coordinates, remote, best.ref, digest), nil
}

func (s *GitHubStore) List(ctx context.Context, language Language, name string) ([]string, error) {
	if language != LanguageGo {
		return nil, fmt.Errorf("librarystore: listing %s libraries is not implemented yet", language)
	}
	tagged, err := s.listTagged(ctx, s.remoteFor(language, name))
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(tagged))
	for _, entry := range tagged {
		result = append(result, entry.version.String())
	}
	return result, nil
}

// taggedVersion is one published semver tag with the commit it resolves to.
type taggedVersion struct {
	version *semver.Version
	ref     string
}

// listTagged reads the remote's semver tags and their commits in one ls-remote,
// newest first. Capturing the commit here — instead of a second ls-remote at
// resolve time — removes the window in which the tag set could change between
// listing and commit lookup.
func (s *GitHubStore) listTagged(ctx context.Context, remote string) ([]taggedVersion, error) {
	out, err := s.output(ctx, "", "ls-remote", "--tags", remote)
	if err != nil {
		return nil, err
	}
	return parseTagListing(out), nil
}

// parseTagListing extracts semver tags from `ls-remote --tags` output. An
// annotated tag appears twice — the tag object and the peeled `^{}` commit —
// and the peeled commit wins; a lightweight tag appears once and its hash is
// already the commit.
func parseTagListing(out string) []taggedVersion {
	type entry struct {
		version *semver.Version
		ref     string
		peeled  bool
	}
	byVersion := map[string]entry{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		ref := fields[1]
		peeled := strings.HasSuffix(ref, "^{}")
		ref = strings.TrimSuffix(ref, "^{}")
		tag := strings.TrimPrefix(ref, "refs/tags/")
		if tag == ref || !strings.HasPrefix(tag, "v") {
			continue
		}
		v, err := semver.NewVersion(strings.TrimPrefix(tag, "v"))
		if err != nil {
			continue
		}
		key := v.String()
		if existing, ok := byVersion[key]; ok && existing.peeled && !peeled {
			continue
		}
		byVersion[key] = entry{version: v, ref: fields[0], peeled: peeled}
	}
	result := make([]taggedVersion, 0, len(byVersion))
	for _, e := range byVersion {
		result = append(result, taggedVersion{version: e.version, ref: e.ref})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].version.GreaterThan(result[j].version) })
	return result
}

func isCommitHash(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// digestAtTag computes the content digest of a published version from a shallow
// clone at its tag, so Resolve honors the Published digest contract with the
// same value Publish recorded.
func (s *GitHubStore) digestAtTag(ctx context.Context, remote, tag string) (string, error) {
	work, err := os.MkdirTemp("", "codefly-library-resolve-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	if err := s.git(ctx, "", "clone", "--quiet", "--depth", "1", "--branch", tag, remote, work); err != nil {
		return "", fmt.Errorf("fetch %s at %s: %w", remote, tag, err)
	}
	return treeDigest(work)
}

func (s *GitHubStore) published(c Coordinates, remote, ref, digest string) Published {
	importPath := goModulePath(remote)
	tag := versionTag(c.Version)
	return Published{
		Coordinates: c,
		ImportPath:  importPath,
		Ref:         ref,
		Location:    remote,
		Digest:      digest,
		InstallHint: fmt.Sprintf("go get %s@%s", importPath, tag),
	}
}

func (s *GitHubStore) tagExists(ctx context.Context, dir, tag string) bool {
	return s.git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/tags/"+tag) == nil
}

// defaultBranch reports the branch the clone checked out — the remote's default
// branch, or the unborn initial branch of an empty repository. Publishing on it
// (rather than forcing "main") keeps the published repository's branch layout
// matching its GitHub default.
func (s *GitHubStore) defaultBranch(ctx context.Context, work string) (string, error) {
	out, err := s.output(ctx, work, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve default branch: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (s *GitHubStore) git(ctx context.Context, dir string, args ...string) error {
	//nolint:gosec // git is invoked with internal subcommands and store-controlled arguments, never a shell.
	command := exec.CommandContext(ctx, "git", gitArgs(dir, args)...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (s *GitHubStore) output(ctx context.Context, dir string, args ...string) (string, error) {
	//nolint:gosec // git is invoked with internal subcommands and store-controlled arguments, never a shell.
	command := exec.CommandContext(ctx, "git", gitArgs(dir, args)...)
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func gitArgs(dir string, args []string) []string {
	if dir == "" {
		return args
	}
	return append([]string{"-C", dir}, args...)
}

// replaceTrackedTree makes the working tree's tracked content equal source: it
// removes every entry except .git, then copies source in. This keeps a release
// commit an exact snapshot of the packaged export, dropping files a prior
// version had and this one does not. A .git directory inside source (a library
// that is itself a git checkout) is skipped — copying it would interleave with
// and corrupt the clone's own .git — and symlinks are rejected so a published
// export is self-contained.
func replaceTrackedTree(work, source string) error {
	entries, err := os.ReadDir(work)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == gitDir {
			continue
		}
		if err := os.RemoveAll(filepath.Join(work, entry.Name())); err != nil {
			return err
		}
	}
	fsys := os.DirFS(source)
	return fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}
		target := filepath.Join(work, filepath.FromSlash(path))
		if entry.IsDir() {
			if entry.Name() == gitDir {
				return fs.SkipDir
			}
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files and directories may be published", path)
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// treeDigest is a deterministic sha256 over the regular files under root, keyed
// by slash-separated relative path and the git-normalized file mode. Git
// preserves only the executable bit through publish and clone, so the digest
// folds each mode to 755/644 — capturing an executable-bit flip (a real change,
// per the docker-build-recipe v2 contract) while staying identical between the
// publish-side source tree and a resolve-side clone. Any .git directory is
// excluded: it is never part of the published content. Reads go through a
// root-scoped filesystem, so a symlink cannot escape root during the walk.
func treeDigest(root string) (string, error) {
	type entry struct {
		path   string
		mode   string
		digest [32]byte
	}
	fsys := os.DirFS(root)
	var entries []entry
	err := fs.WalkDir(fsys, ".", func(path string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if dirEntry.IsDir() {
			if dirEntry.Name() == gitDir {
				return fs.SkipDir
			}
			return nil
		}
		if !dirEntry.Type().IsRegular() {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		info, err := dirEntry.Info()
		if err != nil {
			return err
		}
		mode := "644"
		if info.Mode()&0o111 != 0 {
			mode = "755"
		}
		entries = append(entries, entry{path: path, mode: mode, digest: sha256.Sum256(data)})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	hasher := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(hasher, "%s\x00%s\x00%s\n", e.path, e.mode, hex.EncodeToString(e.digest[:]))
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}
