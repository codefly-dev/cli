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

// PullCmd pulls the latest code from the default branch into every git repo
// under the codefly.dev monorepo root (cli/, core/, proto/, sdk-go/, ...) in
// one step. It is intentionally NON-DESTRUCTIVE: local commits are preserved
// (it merges, never resets), and uncommitted changes are stashed around the
// merge and restored afterward. Nothing local is overwritten — if a merge or
// stash-pop would conflict, that repo is left untouched and reported so you
// can resolve it by hand.
//
// This replaces the ad-hoc "loop over the sibling repos and git pull" shell
// script; the same discovery logic that powers `self build` (walk up to the
// monorepo root) is reused so it works from anywhere in the tree.
var PullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Fast-forward all Codefly repositories to their latest main branches",
	Long: `Pull pulls the latest code from the default branch into every git
repository under the codefly.dev monorepo root — cli/, core/, proto/,
sdk-go/, and any other sibling repo — in a single step.

By default it pulls the codefly module repos — cli/, core/, proto/, sdk-go/.
Use --all to pull every git repository under the monorepo root instead (the
whole workspace). Use --with-agents to ALSO iterate every agent repo under
agents/ (services/*, modules/*, toolboxes/*) — agents/ is not a git repo
itself; each agent is its own GitHub repo. This mirrors
` + "`codefly self build --with-agents`" + `.

It never overrides existing code:
  - Local commits are preserved (it MERGES origin/<branch>, it never resets).
  - Uncommitted changes are stashed before the merge and restored after.
  - If a merge or stash restore would conflict, that repo is left exactly as
    it was and reported, so you can resolve it manually.

Directories that are not git repositories (e.g. agents/, which is not
version-controlled locally) are reported as skipped.

Examples:
  codefly self pull
  codefly self pull --with-agents
  codefly self pull --all --with-agents
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
		all, err := cmd.Flags().GetBool("all")
		if err != nil {
			return fmt.Errorf("cannot read --all: %w", err)
		}
		withAgents, err := cmd.Flags().GetBool("with-agents")
		if err != nil {
			return fmt.Errorf("cannot read --with-agents: %w", err)
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
		scope := "the codefly module repos"
		if all {
			scope = "every repo"
		}
		if withAgents {
			scope += " + agents"
		}
		cli.Info("Pulling %s/%s into %s under %s", remote, branch, scope, root)

		names, err := pullTargets(root, all)
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", root, err)
		}

		// Build the unified target list. agents/ is not a git repo itself —
		// each agent under it is. With --with-agents we expand it into those
		// nested repos; otherwise the bare agents/ entry yields a skip notice.
		var targets []repoTarget
		for _, name := range names {
			if name == "agents" && withAgents {
				continue // expanded below
			}
			targets = append(targets, repoTarget{label: name, path: filepath.Join(root, name)})
		}
		if withAgents {
			agentTargets, err := discoverAgentRepos(root)
			if err != nil {
				return fmt.Errorf("cannot discover agent repositories: %w", err)
			}
			targets = append(targets, agentTargets...)
		}

		// Width the label column to the longest label (agent paths are long).
		width := 12
		for _, t := range targets {
			if len(t.label) > width {
				width = len(t.label)
			}
		}

		var pulled, skipped, failed int
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
				cli.Error("%-*s %v", width, t.label, perr)
				failed++
				failures = append(failures, fmt.Errorf("%s: %w", t.label, perr))
				continue
			}
			cli.Info("%-*s %s", width, t.label, res)
			pulled++
		}

		cli.Info("Done: %d pulled, %d skipped, %d failed", pulled, skipped, failed)
		return errors.Join(failures...)
	},
}

func init() {
	PullCmd.Flags().String("dir", "", "Monorepo root or any directory inside it (default: auto-detect from current directory)")
	PullCmd.Flags().String("branch", "main", "Branch to pull from")
	PullCmd.Flags().String("remote", "origin", "Remote to pull from")
	PullCmd.Flags().Bool("all", false, "Pull every git repo under the monorepo root, not just the codefly module repos")
	PullCmd.Flags().Bool("with-agents", false, "Also pull every agent repo under agents/ (services, modules, toolboxes)")
}

// repoTarget is one repository to pull: an absolute path plus a short label
// for display (e.g. "core" or "agents/services/go-grpc").
type repoTarget struct {
	label string
	path  string
}

// agentCategories are the agents/ subdirectories whose immediate children are
// individual agent git repos. agents/ itself is not version-controlled; each
// agent is its own GitHub repo (service-go-grpc, toolbox-docker, ...).
var agentCategories = []string{"services", "modules", "toolboxes", "applications"}

// discoverAgentRepos returns every agent git repo under root/agents, as
// targets labelled by their path relative to root (so the output reads
// "agents/services/go-grpc"). Sorted for stable, grouped output.
func discoverAgentRepos(root string) ([]repoTarget, error) {
	var targets []repoTarget
	for _, category := range agentCategories {
		base := filepath.Join(root, "agents", category)
		entries, err := os.ReadDir(base)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", base, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(base, e.Name())
			if !isGitRepo(path) {
				continue
			}
			targets = append(targets, repoTarget{
				label: filepath.Join("agents", category, e.Name()),
				path:  path,
			})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].label < targets[j].label })
	return targets, nil
}

// codeflyRepos is the default set pulled by `self pull` — the codefly module
// repos, in dependency-ish order. agents/ is included so it gets a clear
// "not a git repo" notice (it is not version-controlled locally) rather than
// being silently absent. --all ignores this and pulls every repo under root.
var codeflyRepos = []string{"core", "cli", "proto", "sdk-go", "agents"}

// pullTargets returns the directory names to consider, sorted for --all or in
// the curated codeflyRepos order otherwise (skipping ones that don't exist).
func pullTargets(root string, all bool) ([]string, error) {
	if all {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		return names, nil
	}
	var names []string
	for _, name := range codeflyRepos {
		if info, err := os.Stat(filepath.Join(root, name)); err == nil && info.IsDir() {
			names = append(names, name)
		}
	}
	return names, nil
}

// pullRepo merges remote/branch into the repo's current branch without ever
// discarding local work. It fetches, stashes any dirty state, merges, then
// restores the stash. On a merge conflict it aborts cleanly and returns an
// error; the repo is left as it was found.
func pullRepo(ctx context.Context, repo, remote, branch string) (string, error) {
	if out, err := git(ctx, repo, "fetch", "--", remote, branch); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("fetch failed: %s", firstLine(out))
	}

	before, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve current HEAD: %s", firstLine(before))
	}
	before = strings.TrimSpace(before)

	// Stash uncommitted changes (including untracked) so the merge can't touch
	// them; restore afterward. dirty is true only if something was stashed.
	dirty := false
	status, err := git(ctx, repo, "status", "--porcelain")
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("inspect local changes: %s", firstLine(status))
	}
	if strings.TrimSpace(status) != "" {
		if _, err := git(ctx, repo, "stash", "push", "--include-untracked", "-m", "codefly self pull"); err != nil {
			return "", fmt.Errorf("could not stash local changes; left untouched")
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
				return "", errors.Join(
					fmt.Errorf("merge failed: %s", firstLine(mergeOut)),
					fmt.Errorf("could not restore local changes; backup remains in git stash: %s", firstLine(restoreOut)),
				)
			}
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("merge failed; local work restored (%s)", firstLine(mergeOut))
	}

	if dirty {
		if restoreOut, err := git(ctx, repo, "stash", "pop"); err != nil {
			// Merge landed, but reapplying local changes hit conflicts: git
			// left conflict markers in the tree AND retained the stash as a
			// backup. This is not a successful pull: flag it for manual repair.
			return "", fmt.Errorf("merge completed but local changes could not be restored; resolve the working tree and use the backup retained in git stash: %s", firstLine(restoreOut))
		}
	}

	after, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve updated HEAD: %s", firstLine(after))
	}
	after = strings.TrimSpace(after)
	if after == before {
		return "already up to date", nil
	}
	return fmt.Sprintf("updated %s..%s", short(before), short(after)), nil
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
