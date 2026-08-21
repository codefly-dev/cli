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
	if _, err := semver.NewVersion(strings.TrimPrefix(c.Version, "v")); err != nil {
		return Published{}, fmt.Errorf("librarystore: %q is not a semantic version: %w", c.Version, err)
	}
	remote := s.remoteFor(c.Language, c.Name)
	tag := versionTag(c.Version)

	work, err := os.MkdirTemp("", "codefly-library-publish-*")
	if err != nil {
		return Published{}, err
	}
	defer os.RemoveAll(work)

	if err = s.git(ctx, "", "clone", "--quiet", remote, work); err != nil {
		return Published{}, fmt.Errorf("clone %s: %w", remote, err)
	}
	if s.tagExists(ctx, work, tag) {
		return Published{}, fmt.Errorf("librarystore: %s %s is already published (versions are immutable)", c.Name, tag)
	}
	// A fresh library repository is an unborn branch; an existing one already has
	// main. checkout -b creates it on the first publish, checkout switches to it
	// afterwards.
	if err = s.git(ctx, work, "checkout", "main"); err != nil {
		if createErr := s.git(ctx, work, "checkout", "-b", "main"); createErr != nil {
			return Published{}, fmt.Errorf("prepare main branch: %w", createErr)
		}
	}
	if err = replaceTrackedTree(work, artifactDir); err != nil {
		return Published{}, fmt.Errorf("stage artifact: %w", err)
	}
	if err = s.git(ctx, work, "add", "-A"); err != nil {
		return Published{}, err
	}
	message := fmt.Sprintf("release %s %s", c.Name, tag)
	if err = s.git(ctx, work,
		"-c", "user.name="+s.commitName, "-c", "user.email="+s.commitEmail,
		"commit", "--quiet", "-m", message); err != nil {
		return Published{}, fmt.Errorf("commit release: %w", err)
	}
	if err = s.git(ctx, work,
		"-c", "user.name="+s.commitName, "-c", "user.email="+s.commitEmail,
		"tag", "-a", tag, "-m", message); err != nil {
		return Published{}, fmt.Errorf("tag release: %w", err)
	}
	if err = s.git(ctx, work, "push", "--quiet", "origin", "main"); err != nil {
		return Published{}, fmt.Errorf("push branch: %w", err)
	}
	if err = s.git(ctx, work, "push", "--quiet", "origin", tag); err != nil {
		return Published{}, fmt.Errorf("push tag: %w", err)
	}
	commit, err := s.output(ctx, work, "rev-parse", tag+"^{commit}")
	if err != nil {
		return Published{}, err
	}
	digest, err := treeDigest(artifactDir)
	if err != nil {
		return Published{}, err
	}
	return s.published(c, remote, strings.TrimSpace(commit), digest), nil
}

func (s *GitHubStore) Resolve(ctx context.Context, language Language, name, constraint string) (Published, error) {
	if language != LanguageGo {
		return Published{}, fmt.Errorf("librarystore: resolving %s libraries is not implemented yet", language)
	}
	versions, err := s.List(ctx, language, name)
	if err != nil {
		return Published{}, err
	}
	if len(versions) == 0 {
		return Published{}, fmt.Errorf("librarystore: no published versions of %s", name)
	}
	check, err := semver.NewConstraint(strings.TrimSpace(constraint))
	if err != nil {
		return Published{}, fmt.Errorf("librarystore: invalid version constraint %q: %w", constraint, err)
	}
	var best *semver.Version
	for _, candidate := range versions {
		v, parseErr := semver.NewVersion(candidate)
		if parseErr != nil {
			continue
		}
		if check.Check(v) && (best == nil || v.GreaterThan(best)) {
			best = v
		}
	}
	if best == nil {
		return Published{}, fmt.Errorf("librarystore: no published version of %s satisfies %q", name, constraint)
	}
	remote := s.remoteFor(language, name)
	commit, err := s.output(ctx, "", "ls-remote", remote, "refs/tags/"+versionTag(best.String())+"^{}")
	if err != nil {
		return Published{}, err
	}
	ref := strings.TrimSpace(strings.SplitN(strings.TrimSpace(commit), "\t", 2)[0])
	return s.published(Coordinates{Language: language, Name: name, Version: best.String()}, remote, ref, ""), nil
}

func (s *GitHubStore) List(ctx context.Context, language Language, name string) ([]string, error) {
	if language != LanguageGo {
		return nil, fmt.Errorf("librarystore: listing %s libraries is not implemented yet", language)
	}
	remote := s.remoteFor(language, name)
	out, err := s.output(ctx, "", "ls-remote", "--tags", remote)
	if err != nil {
		return nil, err
	}
	var versions []*semver.Version
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		ref := strings.TrimSuffix(fields[1], "^{}")
		tag := strings.TrimPrefix(ref, "refs/tags/")
		if tag == ref || !strings.HasPrefix(tag, "v") {
			continue
		}
		v, err := semver.NewVersion(strings.TrimPrefix(tag, "v"))
		if err != nil {
			continue
		}
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].GreaterThan(versions[j]) })
	seen := map[string]struct{}{}
	result := make([]string, 0, len(versions))
	for _, v := range versions {
		key := v.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	return result, nil
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
// version had and this one does not. os.CopyFS copies regular files and
// directories and rejects symlinks, so a published export is self-contained.
func replaceTrackedTree(work, source string) error {
	entries, err := os.ReadDir(work)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(work, entry.Name())); err != nil {
			return err
		}
	}
	return os.CopyFS(work, os.DirFS(source))
}

// treeDigest is a deterministic sha256 over the regular files under root, keyed
// by slash-separated relative path, so an identical export yields an identical
// digest. It reads through a root-scoped filesystem, so a symlink cannot escape
// root during the walk.
func treeDigest(root string) (string, error) {
	type entry struct {
		path   string
		digest [32]byte
	}
	fsys := os.DirFS(root)
	var entries []entry
	err := fs.WalkDir(fsys, ".", func(path string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if dirEntry.IsDir() || !dirEntry.Type().IsRegular() {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		entries = append(entries, entry{path: path, digest: sha256.Sum256(data)})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	hasher := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(hasher, "%s\x00%s\n", e.path, hex.EncodeToString(e.digest[:]))
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}
