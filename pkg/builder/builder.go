package builder

import (
	"context"

	builderv1 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v1"
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
func DockerBuildContext(_ context.Context, _ *resources.Workspace) (*builderv1.DockerBuildContext, error) {
	return &builderv1.DockerBuildContext{
		DockerRepository: repository,
	}, nil
}

func BuildContextFromDocker(dockerContext *builderv1.DockerBuildContext) *builderv1.BuildContext {
	return &builderv1.BuildContext{
		Kind: &builderv1.BuildContext_DockerBuildContext{
			DockerBuildContext: dockerContext,
		},
	}
}
