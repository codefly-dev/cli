package orchestration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/wool"
	"github.com/moby/patternmatcher"
)

// committedSource is the build context of a pinned release taken from the
// commit its checkout is at rather than from the files on disk.
//
// A module the CLI materialized (modulecache.Owned) is a git checkout of the
// pinned commit, and it is the code of that commit by definition — but it is
// also a directory a local run works in. A run rewrites files the module
// commits (a service that tracks its agent's `.cache/local/*.hash` has them
// rewritten on every start), leaves untracked files, and writes the scratch its
// repository ignores. Hashing those files made the Code class of one pinned
// commit depend on what the CLI's own cache had last been used for, so a render
// refused the very image that commit published as "already published from other
// code", and building from them would push an image that is not the release's.
//
// So both the identity and the build read one copy: the committed tree of the
// context at HEAD, checked out the way a clone checks it out (checkout-index
// applies the repository's attributes and filters, its modes and its links),
// plus the entries the repository ignores that Docker would send. Those are
// carried because an agent writes some of its build inputs there (a generated
// logging configuration the Dockerfile copies), and are recorded in scratch so
// the digest leaves them out: the release's identity is its commit, which a
// fresh clone reproduces and the published label records. A tracked edit and
// an untracked file therefore move neither the identity nor the image.
type committedSource struct {
	// root is the copy of the context; its base name is the context's, which
	// the digest records.
	root string
	// commit is the commit the copy was taken from.
	commit string
	// scratch lists the context-relative, slash-separated paths carried from
	// the working tree because the repository ignores them; the digest leaves
	// them out.
	scratch map[string]bool
	// dir is the temporary directory holding the copy.
	dir string
}

// committedSourceContext copies the committed tree of contextDir, or returns nil
// when contextDir is not a tracked part of a repository with a commit to read —
// the verified-package cache holds no repository, and an assembled context
// under an ignored directory is not in one — in which case the working tree is
// the context, as for a checkout the developer owns.
func committedSourceContext(ctx context.Context, contextDir, outputDir string, recipe *builderv0.DockerBuildRecipe) (*committedSource, error) {
	w := wool.Get(ctx).In("orchestration.committedSourceContext")
	top, prefix, commit, ok := committedLocation(ctx, contextDir)
	if !ok {
		w.Debug("the pinned context is not a committed tree; its files are the context", wool.DirField(contextDir))
		return nil, nil
	}
	dir, err := os.MkdirTemp("", "codefly-build-source-")
	if err != nil {
		return nil, err
	}
	source := &committedSource{commit: commit, scratch: map[string]bool{}, dir: dir}
	if err := source.checkout(ctx, top, prefix); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("check out %s of %s: %w", commit, contextDir, err)
	}
	if err := source.carryIgnored(ctx, contextDir, outputDir, recipe); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("carry the ignored build inputs of %s: %w", contextDir, err)
	}
	return source, nil
}

// committedLocation is the repository top, the context's slash-separated
// prefix within it and the commit at HEAD, when contextDir is a part of a
// repository its rules do not ignore.
func committedLocation(ctx context.Context, contextDir string) (top, prefix, commit string, ok bool) {
	// Exit 1 is "not ignored"; 0 is an ignored context and anything else says
	// there is no repository to read.
	check := exec.CommandContext(ctx, "git", "-C", contextDir, "check-ignore", "-q", ".")
	var exit *exec.ExitError
	if err := check.Run(); err == nil || !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return "", "", "", false
	}
	out, err := exec.CommandContext(ctx, "git", "-C", contextDir, "rev-parse", "--show-toplevel", "--show-prefix", "--verify", "-q", "HEAD^{commit}").Output()
	if err != nil {
		return "", "", "", false
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 3 || lines[0] == "" || lines[2] == "" {
		return "", "", "", false
	}
	return lines[0], strings.TrimSuffix(lines[1], "/"), lines[2], true
}

// checkout writes the committed files under prefix through a private index, so
// neither the checkout's index nor its working tree is touched: a run may be
// using both. The copy lands at <dir>/<base of the repository>/<prefix>, whose
// base name is the context's.
func (source *committedSource) checkout(ctx context.Context, top, prefix string) error {
	resolved, err := filepath.EvalSymlinks(source.dir)
	if err != nil {
		return err
	}
	tree := filepath.Join(resolved, "tree", filepath.Base(top))
	source.root = filepath.Join(tree, filepath.FromSlash(prefix))
	git := func(stdin io.Reader, args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, "git", append([]string{"-C", top}, args...)...) //nolint:gosec // G204: fixed git executable; arguments are this function's own subcommands, a commit id and a literal pathspec, no shell
		command.Env = append(os.Environ(), "GIT_INDEX_FILE="+filepath.Join(resolved, "index"))
		command.Stdin = stdin
		var stderr bytes.Buffer
		command.Stderr = &stderr
		out, runErr := command.Output()
		if runErr != nil {
			return nil, fmt.Errorf("git %s: %w: %s", args[0], runErr, strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}
	if _, err = git(nil, "read-tree", source.commit); err != nil {
		return err
	}
	pathspec := ":(top,literal)" + prefix
	if prefix == "" {
		pathspec = ":(top)"
	}
	paths, err := git(nil, "ls-files", "-z", "--", pathspec)
	if err != nil {
		return err
	}
	if _, err := git(bytes.NewReader(paths), "checkout-index", "-f", "-z", "--stdin", "--prefix="+tree+string(filepath.Separator)); err != nil {
		return err
	}
	// A context whose committed tree is empty still exists, as the directory
	// it is on disk.
	return os.MkdirAll(source.root, 0o755)
}

// carryIgnored copies into the committed copy what the repository ignores in the
// working context and the recipe's ignore policy lets Docker send, recording
// the outermost path each entry added so the digest can leave it out. An
// ignored path is never a committed one, so nothing committed is overwritten.
func (source *committedSource) carryIgnored(ctx context.Context, contextDir, outputDir string, recipe *builderv0.DockerBuildRecipe) error {
	ignored := sourceIgnoredPaths(ctx, contextDir)
	if len(ignored) == 0 {
		return nil
	}
	// Docker reads the context root's ignore file from the context it is sent,
	// so the policy is the committed one.
	patterns, err := contextIgnorePatterns(outputDir, source.root, recipe)
	if err != nil {
		return err
	}
	ignore, err := patternmatcher.New(patterns)
	if err != nil {
		return fmt.Errorf("ignore policy of %s: %w", recipe.GetName(), err)
	}
	for relative := range ignored {
		added, ok := source.outermostAdded(relative)
		if !ok {
			// The commit holds this path: what is committed is the context.
			continue
		}
		source.scratch[added] = true
		err := filepath.WalkDir(filepath.Join(contextDir, filepath.FromSlash(relative)), func(from string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			within, err := filepath.Rel(contextDir, from)
			if err != nil {
				return err
			}
			if dropped, err := ignore.MatchesOrParentMatches(within); err != nil || dropped {
				if err == nil && entry.IsDir() && !ignore.Exclusions() {
					return filepath.SkipDir
				}
				return err
			}
			return copyContextEntry(from, filepath.Join(source.root, within), entry)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// outermostAdded is the outermost ancestor of relative (or relative itself)
// that the committed copy does not hold, which is the one path that leaves out
// everything carrying relative adds; false when the copy holds relative.
func (source *committedSource) outermostAdded(relative string) (string, bool) {
	parts := strings.Split(relative, "/")
	for index := range parts {
		candidate := path.Join(parts[:index+1]...)
		if _, err := os.Lstat(filepath.Join(source.root, filepath.FromSlash(candidate))); err != nil {
			return candidate, true
		}
	}
	return "", false
}

func copyContextEntry(from, to string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		return os.MkdirAll(to, 0o755)
	case info.Mode()&os.ModeSymlink != 0:
		target, linkErr := os.Readlink(from)
		if linkErr != nil {
			return linkErr
		}
		if err = os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		return os.Symlink(target, to)
	case !info.Mode().IsRegular():
		return nil
	}
	if err = os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Close())
}
