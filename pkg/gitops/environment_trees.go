package gitops

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/workingrender"
	"github.com/codefly-dev/core/resources"
)

// EnvironmentModuleTrees lists the module trees in workspace that say what
// env delivers, in the order a reader lets the later win: the environment's
// working render (workingrender.Modules), what a publish would deliver, then
// its own delivered tree, `<gitops path>/modules`, but only when this
// checkout is the one that delivers it. Another environment's tree is never
// among them: a tree under a path env does not deliver to is another
// environment's, whatever overlays it carries.
//
// Whether the checkout delivers env is decided by deliversFromThisCheckout,
// per environment. It was once decided by comparing the environment's
// `gitops.repo-url` against the workspace's default, which required a
// workspace-level `gitops:` block to exist to decide anything at all: with no
// default, every environment's checkout tree was read, and read last, so a
// stale committed tree outranked a fresh render. That is the shape of the one
// workspace this matters most for.
func EnvironmentModuleTrees(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) ([]string, error) {
	trees := make([]string, 1, 2)
	trees[0] = workingrender.Modules(workspace.Dir(), env.Name)
	defaults, err := environments.WorkspaceGitops(workspace)
	if err != nil {
		return nil, err
	}
	delivers, err := deliversFromThisCheckout(ctx, workspace, env, defaults)
	if err != nil {
		return nil, err
	}
	if !delivers {
		return trees, nil
	}
	gitopsPath, err := validateRelativePath(gitopsPathOf(env, defaults))
	if err != nil {
		return nil, fmt.Errorf("gitops.path: %w", err)
	}
	return append(trees, filepath.Join(workspace.Dir(), filepath.FromSlash(deliveryModulesPath(gitopsPath)))), nil
}

// deliversFromThisCheckout answers whether what workspace's own checkout holds
// under env's gitops path is env's delivered tree, or another branch's.
//
// An environment that names no delivery repository, in a workspace that
// declares no default, delivers nowhere else: the tree on disk is the only one
// there is, and it is read as before.
//
// When a delivery repository is named, the repository alone decides nothing.
// Environments commonly deliver into the very repository the workspace is
// checked out from, each on its own branch — so the tree under env's gitops
// path is env's delivered tree only while this checkout is on env's delivery
// branch. On any other branch it is whatever that branch last held, and
// reading it would let it outrank a fresh render, which is precisely what
// `codefly authority` and `codefly doctor solution --env` are asked to show.
// The branch is read from this checkout, with no network and no credential.
//
// A checkout whose branch cannot be read — no repository, or a detached HEAD —
// is refused by name. Neither answer would be known to be true, and guessing
// either way decides silently what the reader asked about.
func deliversFromThisCheckout(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, defaults *environments.EnvironmentGitops) (bool, error) {
	repoURL, branch := deliveryRepositoryOf(env, defaults)
	if repoURL == "" {
		return true, nil
	}
	head, err := checkoutBranch(ctx, workspace.Dir())
	if err != nil {
		return false, fmt.Errorf("environment %q delivers to %s on branch %q, and this checkout's own branch could not be read, so what %s holds under its gitops path can neither be read as %q's nor ruled out: %w",
			env.Name, repoURL, branch, workspace.Dir(), env.Name, err)
	}
	return head == branch, nil
}

// deliveryRepositoryOf is where an environment delivers, read exactly as
// resolveGitops reads it so that what this excludes and what a publish writes
// can never disagree: the environment's own block when it names a repository,
// the workspace's default when it does not (an environment may override the
// path alone, in the workspace's repository, on its branch). The branch
// defaults as the publish's does. Both are empty when nothing names a
// delivery repository.
func deliveryRepositoryOf(env *environments.Environment, defaults *environments.EnvironmentGitops) (repoURL, branch string) {
	if env != nil && env.Gitops != nil && env.Gitops.RepoURL != "" {
		repoURL, branch = env.Gitops.RepoURL, env.Gitops.Branch
	} else if defaults != nil {
		repoURL, branch = defaults.RepoURL, defaults.Branch
	}
	if strings.TrimSpace(repoURL) == "" {
		return "", ""
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		branch = DefaultDeliveryBranch
	}
	return repoURL, branch
}

// checkoutBranch is the branch dir's checkout is on, read with no network.
//
// `symbolic-ref` and not `rev-parse --abbrev-ref`, because the two differ
// exactly where it matters. On a detached HEAD, rev-parse prints the literal
// "HEAD" and succeeds — a branch name no environment can declare, but still a
// value a comparison silently answers with "not this one" rather than "not
// known". symbolic-ref fails there, which is the honest answer. It also reads
// a branch that has no commit yet, which rev-parse refuses.
func checkoutBranch(ctx context.Context, dir string) (string, error) {
	branch, err := gitCommand(ctx, dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	if branch = strings.TrimSpace(branch); branch == "" {
		return "", fmt.Errorf("%s is on no named branch", dir)
	}
	return branch, nil
}
