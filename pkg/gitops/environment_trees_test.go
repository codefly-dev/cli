package gitops

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/workingrender"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The condition this replaced could only decide anything when the workspace
// declared a top-level `gitops:` default to compare an environment's own
// repo-url against: with none, the checkout's tree was appended for every
// environment, and appended last, which is the position that wins. That is
// not a hypothetical workspace — environments commonly deliver into the very
// repository they are declared in, each on its own branch, and declare no
// default at all. TestAnotherEnvironmentsTreeNeverSpeaksForThisOne cannot
// reach it: its fixture declares no repo-url, so it never evaluates the
// branch. This one declares one.
func TestAnEnvironmentsCheckoutTreeIsReadOnlyOnItsDeliveryBranch(t *testing.T) {
	workspace := writeCellWorkspace(t, [3]string{resources.WorkspaceConfigurationName, "environments:\n",
		"environments:\n  - name: blue\n    namespace: acme\n    gitops:\n      repo-url: https://github.com/acme/example.git\n      path: deployments/environments/blue\n      branch: blue-delivery\n"})
	root := workspace.Dir()
	runGit(t, root, "init", "-q", "--initial-branch=main")
	blue := selectedEnvironment(t, workspace, "blue")

	trees, err := EnvironmentModuleTrees(context.Background(), workspace, blue)
	require.NoError(t, err)
	require.Equal(t, []string{workingrender.Modules(root, "blue")}, trees,
		"a checkout on main holds no Blue delivery, so nothing under Blue's gitops path speaks for Blue")

	// On Blue's own delivery branch the same tree is Blue's delivered tree,
	// and is read.
	runGit(t, root, "checkout", "-q", "-b", "blue-delivery")
	trees, err = EnvironmentModuleTrees(context.Background(), workspace, blue)
	require.NoError(t, err)
	require.Equal(t, []string{workingrender.Modules(root, "blue"),
		filepath.Join(root, "deployments", "environments", "blue", "modules")}, trees)

	// An environment naming no delivery repository, in a workspace declaring
	// no default, delivers nowhere else: its tree on disk is read as before,
	// and no branch is consulted for it.
	staging, err := EnvironmentModuleTrees(context.Background(), workspace, selectedEnvironment(t, workspace, "staging"))
	require.NoError(t, err)
	require.Equal(t, []string{workingrender.Modules(root, "staging"), filepath.Join(root, "deployments", "modules")}, staging)
}

// A branch that cannot be read is refused by name. Answering either way would
// decide silently what the reader asked about: include a tree that may be
// another branch's, or hide the only tree there is.
func TestADeliveryBranchThatCannotBeReadIsRefusedNotGuessed(t *testing.T) {
	workspace := writeCellWorkspace(t, [3]string{resources.WorkspaceConfigurationName, "environments:\n",
		"environments:\n  - name: blue\n    namespace: acme\n    gitops:\n      repo-url: https://github.com/acme/example.git\n      path: deployments/environments/blue\n      branch: blue-delivery\n"})
	_, err := EnvironmentModuleTrees(context.Background(), workspace, selectedEnvironment(t, workspace, "blue"))
	require.ErrorContains(t, err, `environment "blue" delivers to https://github.com/acme/example.git on branch "blue-delivery"`)
	require.ErrorContains(t, err, "this checkout's own branch could not be read")

	// A detached HEAD is the same refusal: git names it "HEAD", which is no
	// branch an environment can declare.
	runGit(t, workspace.Dir(), "init", "-q", "--initial-branch=main")
	runGit(t, workspace.Dir(), "add", "-A")
	runGit(t, workspace.Dir(), "commit", "-q", "-m", "the workspace")
	runGit(t, workspace.Dir(), "checkout", "-q", "--detach", "HEAD")
	_, err = EnvironmentModuleTrees(context.Background(), workspace, selectedEnvironment(t, workspace, "blue"))
	require.ErrorContains(t, err, "this checkout's own branch could not be read")
}
