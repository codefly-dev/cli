package builder

import (
	"context"
	"fmt"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
)

var repository = "codefly-dev"

func SetRepository(repo string) {
	repository = repo
}

func DockerBuildContext(ctx context.Context, workspace *resources.Workspace) (*builderv0.DockerBuildContext, error) {
	repo := repository
	if workspace.Layout != resources.LayoutKindFlat {
		repo = fmt.Sprintf("%s/%s", repo, workspace.Name)
	}
	return &builderv0.DockerBuildContext{
		DockerRepository: repo,
	}, nil
}

func BuildContextFromDocker(dockerContext *builderv0.DockerBuildContext) *builderv0.BuildContext {
	return &builderv0.BuildContext{
		Kind: &builderv0.BuildContext_DockerBuildContext{
			DockerBuildContext: dockerContext,
		},
	}
}
