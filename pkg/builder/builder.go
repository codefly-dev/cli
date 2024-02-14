package builder

import (
	"context"

	protobuild "github.com/codefly-dev/cli/pkg/builder/clients/build"
	"github.com/codefly-dev/core/wool"
	"google.golang.org/grpc"
)

type Repository interface {
}

type PlatformRepo struct {
	client protobuild.BuildServiceClient
}

func NewService(ctx context.Context) (*PlatformRepo, error) {
	w := wool.Get(ctx).In("NewService")
	// create the gRPC connection
	conn, err := grpc.Dial("localhost:32933", grpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	client := protobuild.NewBuildServiceClient(conn)
	v, err := client.Version(context.Background(), &protobuild.VersionRequest{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot get version")
	}
	w.Debug("Connected to builder", wool.Field("version", v.Version))
	return &PlatformRepo{client: client}, nil
}
