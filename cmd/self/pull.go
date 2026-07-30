package self

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/monorepo"
	"github.com/spf13/cobra"
)

// PullCmd pulls the latest code into the repositories that define the local
// Codefly runtime: cli, core, llm, and canonical plugin checkouts. It is
// intentionally NON-DESTRUCTIVE: local commits are preserved (it merges, never
// resets), and uncommitted changes are stashed around the merge and restored
// afterward. Nothing local is overwritten — if a merge or stash-pop would
// conflict, that repository is left untouched and reported for manual repair.
var PullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Fast-forward the CLI, Core, LLM, and plugins to their latest main branches",
	Long: `Pull updates exactly the repositories that define the local Codefly
runtime: cli/, core/, llm/, and canonical plugin repositories.

Plugins are discovered by a top-level agent.codefly.yaml, the same boundary
used by ` + "`codefly self build --with-agents`" + `. A checkout is canonical
when its directory name matches its origin repository name; duplicate worktrees
and task-specific clones are not touched. SDKs, templates, examples, websites,
release repositories, and other workspace siblings are outside this command's
scope.

It never overrides existing code:
  - Repositories on another branch or detached HEAD are skipped.
  - Local commits are preserved (it MERGES origin/<branch>, it never resets).
  - Uncommitted changes are stashed before the merge and restored after.
  - If a merge or stash restore would conflict, that repo is left exactly as
    it was and reported, so you can resolve it manually.

Examples:
  codefly self pull
  codefly self pull --branch develop
  codefly self pull --remote upstream
  codefly self pull --dir ~/Development/deus/codefly.dev`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		dir, err := cmd.Flags().GetString("dir")
		if err != nil {
			return fmt.Errorf("cannot read --dir: %w", err)
		}
		branch, err := cmd.Flags().GetString("branch")
		if err != nil {
			return fmt.Errorf("cannot read --branch: %w", err)
		}
		remote, err := cmd.Flags().GetString("remote")
		if err != nil {
			return fmt.Errorf("cannot read --remote: %w", err)
		}
		if remote == "" || strings.HasPrefix(remote, "-") {
			return fmt.Errorf("invalid --remote %q", remote)
		}
		if branch == "" || strings.HasPrefix(branch, "-") {
			return fmt.Errorf("invalid --branch %q", branch)
		}

		root, err := resolveMonorepoRoot(dir)
		if err != nil {
			return fmt.Errorf("cannot locate the codefly monorepo root: %w", err)
		}
		cli.Info("Pulling %s/%s into CLI, Core, LLM, and plugins under %s", remote, branch, root)

		names, err := pullTargets(ctx, root)
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", root, err)
		}

		targets := make([]repoTarget, 0, len(names))
		for _, name := range names {
			targets = append(targets, repoTarget{label: name, path: filepath.Join(root, name)})
		}

		// Width the label column to the longest plugin name.
		width := 12
		for _, t := range targets {
			if len(t.label) > width {
				width = len(t.label)
			}
		}

		var updated, unchanged, skipped, failed int
		var failures []error
		for _, t := range targets {
			if err := ctx.Err(); err != nil {
				failures = append(failures, err)
				break
			}
			if !isGitRepo(t.path) {
				// Only flag module-looking dirs so we don't spam about
				// node_modules, bin, etc. A bare agents/ lands here.
				if looksLikeModule(t.path) {
					cli.Warning("%-*s skipped (not a git repository)", width, t.label)
					skipped++
				}
				continue
			}
			res, perr := pullRepo(ctx, t.path, remote, branch)
			if perr != nil {
				// A vanished upstream (the GitHub repo was deleted/renamed while
				// the local checkout lingers, e.g. proto/) is not a real
				// failure — skip it quietly rather than failing the whole run.
				if errors.Is(perr, errRemoteMissing) {
					cli.Warning("%-*s skipped (remote no longer exists)", width, t.label)
					skipped++
					continue
				}
				cli.Error("%-*s %v", width, t.label, perr)
				failed++
				failures = append(failures, fmt.Errorf("%s: %w", t.label, perr))
				continue
			}
			switch res.kind {
			case pullResultUpdated:
				cli.Info("%-*s %s", width, t.label, res.message)
				updated++
			case pullResultUnchanged:
				cli.Info("%-*s %s", width, t.label, res.message)
				unchanged++
			case pullResultSkipped:
				cli.Warning("%-*s %s", width, t.label, res.message)
				skipped++
			}
		}

		cli.Info(
			"Done: %d updated, %d already up to date, %d skipped, %d failed",
			updated,
			unchanged,
			skipped,
			failed,
		)
		return errors.Join(failures...)
	},
}

func init() {
	PullCmd.Flags().String("dir", "", "Monorepo root or any directory inside it (default: auto-detect from current directory)")
	PullCmd.Flags().String("branch", "main", "Branch to pull from")
	PullCmd.Flags().String("remote", "origin", "Remote to pull from")
}

// repoTarget is one repository to pull: an absolute path plus a short label
// for display (e.g. "core" or "service-go-grpc").
type repoTarget struct {
	label string
	path  string
}

type pullResultKind uint8

const (
	pullResultUpdated pullResultKind = iota
	pullResultUnchanged
	pullResultSkipped
)

type pullResult struct {
	kind    pullResultKind
	message string
}

var pullFoundationRepos = []string{"cli", "core", "llm"}

// pullTargets returns exactly the canonical runtime repositories: cli, core,
// llm, and top-level plugin checkouts. A plugin is identified by its
// agent.codefly.yaml. Requiring the checkout directory to match the origin
// repository name excludes task-specific clones and duplicate worktrees.
func pullTargets(ctx context.Context, root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	selected := make(map[string]struct{})
	for _, name := range pullFoundationRepos {
		if isGitRepo(filepath.Join(root, name)) {
			selected[name] = struct{}{}
		}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if isCanonicalPluginRepo(ctx, filepath.Join(root, e.Name()), e.Name()) {
			selected[e.Name()] = struct{}{}
		}
	}

	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func isCanonicalPluginRepo(ctx context.Context, dir, name string) bool {
	if !isGitRepo(dir) {
		return false
	}
	manifest, err := os.Stat(filepath.Join(dir, "agent.codefly.yaml"))
	if err != nil || manifest.IsDir() {
		return false
	}
	out, err := git(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return false
	}
	return remoteRepositoryName(out) == name
}

func remoteRepositoryName(remote string) string {
	remote = strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(remote), "/"), ".git")
	if separator := strings.LastIndexAny(remote, "/:"); separator >= 0 {
		return remote[separator+1:]
	}
	return remote
}

// errRemoteMissing marks a fetch that failed because the upstream repository
// no longer exists (deleted/renamed/made private) — as opposed to a network
// or auth error. These are reported as skips, not failures.
var errRemoteMissing = errors.New("remote repository no longer exists")

// isRemoteMissing reports whether git fetch output indicates the remote
// repository is simply gone. Covers GitHub's 404 message plus the generic
// "could not read from remote repository" that follows a dead SSH remote.
func isRemoteMissing(out string) bool {
	lower := strings.ToLower(out)
	for _, marker := range []string{
		"repository not found",
		"could not read from remote repository",
		"does not appear to be a git repository",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// pullRepo merges remote/branch only when the repository is already checked
// out on branch. Feature branches and detached HEADs are skipped before fetch,
// stash, or merge. For the selected branch, local work remains protected by a
// stash around the merge.
func pullRepo(ctx context.Context, repo, remote, branch string) (pullResult, error) {
	current, err := git(ctx, repo, "branch", "--show-current")
	if err != nil {
		return pullResult{}, fmt.Errorf("resolve current branch: %s", firstLine(current))
	}
	current = strings.TrimSpace(current)
	if current != branch {
		if current == "" {
			current = "detached HEAD"
		}
		return pullResult{
			kind:    pullResultSkipped,
			message: fmt.Sprintf("skipped (on %s; expected branch %s)", current, branch),
		}, nil
	}

	if out, err := git(ctx, repo, "fetch", "--", remote, branch); err != nil {
		if ctx.Err() != nil {
			return pullResult{}, ctx.Err()
		}
		if isRemoteMissing(out) {
			return pullResult{}, fmt.Errorf("%w: %s", errRemoteMissing, firstLine(out))
		}
		return pullResult{}, fmt.Errorf("fetch failed: %s", firstLine(out))
	}

	before, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return pullResult{}, fmt.Errorf("resolve current HEAD: %s", firstLine(before))
	}
	before = strings.TrimSpace(before)

	// Stash uncommitted changes (including untracked) so the merge can't touch
	// them; restore afterward. dirty is true only if something was stashed.
	dirty := false
	status, err := git(ctx, repo, "status", "--porcelain")
	if err != nil {
		if ctx.Err() != nil {
			return pullResult{}, ctx.Err()
		}
		return pullResult{}, fmt.Errorf("inspect local changes: %s", firstLine(status))
	}
	if strings.TrimSpace(status) != "" {
		if _, err := git(ctx, repo, "stash", "push", "--include-untracked", "-m", "codefly self pull"); err != nil {
			return pullResult{}, fmt.Errorf("could not stash local changes; left untouched")
		}
		dirty = true
	}

	mergeOut, mergeErr := git(ctx, repo, "merge", "--no-edit", "FETCH_HEAD")
	if mergeErr != nil {
		// Cancellation must not prevent rollback: use a fresh bounded context to
		// abort the merge and restore the user's stashed work.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = git(cleanupCtx, repo, "merge", "--abort")
		if dirty {
			if restoreOut, restoreErr := git(cleanupCtx, repo, "stash", "pop"); restoreErr != nil {
				return pullResult{}, errors.Join(
					fmt.Errorf("merge failed: %s", firstLine(mergeOut)),
					fmt.Errorf("could not restore local changes; backup remains in git stash: %s", firstLine(restoreOut)),
				)
			}
		}
		if err := ctx.Err(); err != nil {
			return pullResult{}, err
		}
		return pullResult{}, fmt.Errorf("merge failed; local work restored (%s)", firstLine(mergeOut))
	}

	if dirty {
		if restoreOut, err := git(ctx, repo, "stash", "pop"); err != nil {
			// Merge landed, but reapplying local changes hit conflicts: git
			// left conflict markers in the tree AND retained the stash as a
			// backup. This is not a successful pull: flag it for manual repair.
			return pullResult{}, fmt.Errorf("merge completed but local changes could not be restored; resolve the working tree and use the backup retained in git stash: %s", firstLine(restoreOut))
		}
	}

	after, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return pullResult{}, fmt.Errorf("resolve updated HEAD: %s", firstLine(after))
	}
	after = strings.TrimSpace(after)
	if after == before {
		return pullResult{kind: pullResultUnchanged, message: "already up to date"}, nil
	}
	return pullResult{
		kind:    pullResultUpdated,
		message: fmt.Sprintf("updated %s..%s", short(before), short(after)),
	}, nil
}

// git runs a git command in repo and returns combined output.
func git(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// looksLikeModule reports whether dir is a codefly module worth mentioning when
// it isn't a git repo — it has a go.mod or a codefly manifest. Keeps the
// "skipped" notice limited to real modules (agents/) rather than every folder.
func looksLikeModule(dir string) bool {
	for _, marker := range []string{"go.mod", "agent.codefly.yaml", "module.codefly.yaml", "workspace.codefly.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	// agents/ holds nested modules rather than a top-level manifest.
	if filepath.Base(dir) == "agents" {
		return true
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	first, _, _ := strings.Cut(s, "\n")
	return first
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// resolveMonorepoRoot finds the codefly.dev monorepo root: the directory that
// contains the cli module. If dir is given it is used as the search start;
// otherwise the search starts at the current directory and walks up. Reuses
// isCLIModule so it agrees with `self build` on what "the CLI" is.
func resolveMonorepoRoot(dir string) (string, error) {
	start := dir
	if start == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		start = cwd
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	if root := monorepo.FindRoot(abs); root != "" {
		return root, nil
	}
	return "", fmt.Errorf("no codefly monorepo root found from %s (use --dir)", abs)
}
