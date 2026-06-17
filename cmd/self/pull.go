package self

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
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
	Short: "Pull the latest main into every codefly repo (non-destructive)",
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
	Run: func(cmd *cobra.Command, args []string) {
		dir, _ := cmd.Flags().GetString("dir")
		branch, _ := cmd.Flags().GetString("branch")
		remote, _ := cmd.Flags().GetString("remote")
		all, _ := cmd.Flags().GetBool("all")
		withAgents, _ := cmd.Flags().GetBool("with-agents")

		root, err := resolveMonorepoRoot(dir)
		if err != nil {
			cli.Error("Cannot locate the codefly monorepo root: %v", err)
			cli.ExitError()
			return
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
			cli.Error("Cannot read %s: %v", root, err)
			cli.ExitError()
			return
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
			targets = append(targets, discoverAgentRepos(root)...)
		}

		// Width the label column to the longest label (agent paths are long).
		width := 12
		for _, t := range targets {
			if len(t.label) > width {
				width = len(t.label)
			}
		}

		var pulled, skipped, failed int
		for _, t := range targets {
			if !isGitRepo(t.path) {
				// Only flag module-looking dirs so we don't spam about
				// node_modules, bin, etc. A bare agents/ lands here.
				if looksLikeModule(t.path) {
					cli.Warning("%-*s skipped (not a git repository)", width, t.label)
					skipped++
				}
				continue
			}
			res, perr := pullRepo(t.path, remote, branch)
			if perr != nil {
				cli.Error("%-*s %v", width, t.label, perr)
				failed++
				continue
			}
			cli.Info("%-*s %s", width, t.label, res)
			pulled++
		}

		cli.Info("Done: %d pulled, %d skipped, %d failed", pulled, skipped, failed)
		if failed > 0 {
			cli.ExitError()
		}
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
func discoverAgentRepos(root string) []repoTarget {
	var targets []repoTarget
	for _, category := range agentCategories {
		base := filepath.Join(root, "agents", category)
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
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
	return targets
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
func pullRepo(repo, remote, branch string) (string, error) {
	if out, err := git(repo, "fetch", remote, branch); err != nil {
		return "", fmt.Errorf("fetch failed: %s", firstLine(out))
	}

	before, _ := git(repo, "rev-parse", "HEAD")
	before = strings.TrimSpace(before)

	// Stash uncommitted changes (including untracked) so the merge can't touch
	// them; restore afterward. dirty is true only if something was stashed.
	dirty := false
	if status, _ := git(repo, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		if _, err := git(repo, "stash", "push", "--include-untracked", "-m", "codefly self pull"); err != nil {
			return "", fmt.Errorf("could not stash local changes; left untouched")
		}
		dirty = true
	}

	mergeOut, mergeErr := git(repo, "merge", "--no-edit", "FETCH_HEAD")
	if mergeErr != nil {
		// Abort so the working tree is exactly as we found it, then restore.
		_, _ = git(repo, "merge", "--abort")
		if dirty {
			_, _ = git(repo, "stash", "pop")
		}
		return "", fmt.Errorf("merge conflict; left untouched (%s)", firstLine(mergeOut))
	}

	restoreNote := ""
	if dirty {
		if _, err := git(repo, "stash", "pop"); err != nil {
			// Merge landed, but reapplying local changes hit conflicts: git
			// left conflict markers in the tree AND retained the stash as a
			// backup. Nothing is lost — flag it loudly for manual resolution.
			restoreNote = " (merged, but your local changes conflict — resolve markers; backup kept in `git stash`)"
		}
	}

	after, _ := git(repo, "rev-parse", "HEAD")
	after = strings.TrimSpace(after)
	if after == before {
		return "already up to date" + restoreNote, nil
	}
	return fmt.Sprintf("updated %s..%s", short(before), short(after)) + restoreNote, nil
}

// git runs a git command in repo and returns combined output.
func git(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func isGitRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
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
	for d := abs; ; {
		// Inside the cli module itself: root is its parent.
		if isCLIModule(d) {
			return filepath.Dir(d), nil
		}
		// At the monorepo root: it has a cli/ child module.
		if isCLIModule(filepath.Join(d, "cli")) {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return "", fmt.Errorf("no codefly monorepo root found from %s (use --dir)", abs)
}
