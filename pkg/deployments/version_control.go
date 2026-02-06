package deployments

import (
	"context"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/wool"
	git "github.com/go-git/go-git/v5"
)

func InitRepository(ctx context.Context, workspace *resources.Workspace) error {
	w := wool.Get(ctx).In("InitRepository")
	// Initializes a new Git repository
	_, err := git.PlainInit(workspace.Dir(), false)
	if err != nil {
		return w.Wrapf(err, "cannot initialize git repository")
	}

	//Use os/exec to add the submodule
	//cmd := exec.Command("git", "-C", workspace.KustomizeDir(), "submodule", "add", "./_deployments", "_deployments")
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
