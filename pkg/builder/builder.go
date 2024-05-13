package builder

import (
	"context"
	"fmt"

	builderv0 "github.com/codefly-dev/core/generated/go/services/builder/v0"
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

type Repository interface {
}

//
//type PlatformRepo struct {
//	client protobuild.BuildServiceClient
//}
//
//func NewService(ctx context.Context) (*PlatformRepo, error) {
//	w := wool.Get(ctx).In("NewService")
//	// create the gRPC connection
//	conn, err := grpc.Dial("localhost:32933", grpc.WithInsecure())
//	if err != nil {
//		return nil, err
//	}
//	client := protobuild.NewBuildServiceClient(conn)
//	v, err := client.Version(context.Background(), &protobuild.VersionRequest{})
//	if err != nil {
//		return nil, w.Wrapf(err, "cannot get version")
//	}
//	w.Debug("Connected to builder", wool.Field("version", v.Version))
//	return &PlatformRepo{client: client}, nil
//}
