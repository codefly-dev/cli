package builder

import (
	"context"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
)

var repository = "codefly-dev"

func SetRepository(repo string) {
	repository = repo
}

// DockerBuildContext is the repository a service's image is pushed under: the
// registry alone. The agent names the image <repository>/<module>/<service> and
// tags it with the service's version, so an image belongs to the module that owns
// the code, never to the workspace that composed it: the same module release
// composed by two workspaces, or delivered to two environments, is one image.
func DockerBuildContext(_ context.Context, _ *resources.Workspace) (*builderv0.DockerBuildContext, error) {
	return &builderv0.DockerBuildContext{
		DockerRepository: repository,
	}, nil
}

func BuildContextFromDocker(dockerContext *builderv0.DockerBuildContext) *builderv0.BuildContext {
	return &builderv0.BuildContext{
		Kind: &builderv0.BuildContext_DockerBuildContext{
			DockerBuildContext: dockerContext,
		},
	}
}
