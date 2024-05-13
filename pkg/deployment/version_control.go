package deployment

import (
	"context"
	"path"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	git "github.com/go-git/go-git/v5"
)

func Dir(ctx context.Context, workspace *resources.Workspace) string {
	return path.Join(workspace.Dir(), "deployments")
}

func InitRepository(ctx context.Context, workspace *resources.Workspace) error {
	w := wool.Get(ctx).In("InitRepository")
	// Initializes a new Git repository
	_, err := git.PlainInit(workspace.Dir(), false)
	if err != nil {
		return w.Wrapf(err, "cannot initialize git repository")
	}

	deploymentPath := Dir(ctx, workspace)

	// Initialize the submodule repository
	_, err = git.PlainInit(deploymentPath, false)
	if err != nil {
		return w.Wrapf(err, "cannot initialize git repository for deployment")
	}

	//Use os/exec to add the submodule
	//cmd := exec.Command("git", "-C", workspace.Dir(), "submodule", "add", "./_deployments", "_deployments")
	//err = cmd.Run()
	//if err != nil {
	//	return w.Wrapf(err, "cannot add _deployments as a submodule")
	//}
	return nil
}

func SetupBaseVersionControl(ctx context.Context, workspace *resources.Workspace) error {
	w := wool.Get(ctx).In("SetupBaseVersionControl")
	w.Debug("creating a new git repository")
	err := InitRepository(ctx, workspace)
	if err != nil {
		return w.Wrapf(err, "cannot initialize git repository")
	}
	return nil
}
