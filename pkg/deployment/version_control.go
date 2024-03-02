package deployment

import (
	"context"
	"path"

	"github.com/codefly-dev/core/configurations"
	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
	"github.com/codefly-dev/core/wool"
	git "github.com/go-git/go-git/v5"
)

func Dir(ctx context.Context, project *configurations.Project) string {
	return path.Join(project.Dir(), "deployments")
}

func DirFor(ctx context.Context, project *configurations.Project, kind builderv0.DeploymentKind) string {
	var sub string
	switch kind {
	case builderv0.DeploymentKind_KUSTOMIZE:
		sub = "kustomize"
	}
	return path.Join(Dir(ctx, project), sub)
}

func InitRepository(ctx context.Context, project *configurations.Project) error {
	w := wool.Get(ctx).In("InitRepository")
	// Initializes a new Git repository
	_, err := git.PlainInit(project.Dir(), false)
	if err != nil {
		return w.Wrapf(err, "cannot initialize git repository")
	}

	deploymentPath := Dir(ctx, project)

	// Initialize the submodule repository
	_, err = git.PlainInit(deploymentPath, false)
	if err != nil {
		return w.Wrapf(err, "cannot initialize git repository for deployment")
	}

	//Use os/exec to add the submodule
	//cmd := exec.Command("git", "-C", project.Dir(), "submodule", "add", "./_deployments", "_deployments")
	//err = cmd.Run()
	//if err != nil {
	//	return w.Wrapf(err, "cannot add _deployments as a submodule")
	//}
	return nil
}

func SetupBaseVersionControl(ctx context.Context, project *configurations.Project) error {
	w := wool.Get(ctx).In("SetupBaseVersionControl")
	w.Debug("creating a new git repository")
	err := InitRepository(ctx, project)
	if err != nil {
		return w.Wrapf(err, "cannot initialize git repository")
	}
	return nil
}
