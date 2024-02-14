package builder

import (
	"context"

	protoorganization "github.com/codefly-dev/cli/pkg/builder/clients/organization"
	"github.com/codefly-dev/core/wool"
	"google.golang.org/grpc"
)

type OrganizationRepository interface {
}

type PlatformOrganizationRepo struct {
	client protoorganization.OrganizationServiceClient
}

func NewOrganizationService(ctx context.Context) (*PlatformOrganizationRepo, error) {
	w := wool.Get(ctx).In("NewService")
	// create the gRPC connection
	conn, err := grpc.Dial("localhost:32403", grpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	client := protoorganization.NewOrganizationServiceClient(conn)
	v, err := client.Version(context.Background(), &protoorganization.VersionRequest{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot get version")
	}
	w.Debug("Connected to builder", wool.Field("version", v.Version))
	return &PlatformOrganizationRepo{client: client}, nil
}
