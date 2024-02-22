package platform

import (
	"context"
	"net/url"

	api "github.com/codefly-dev/cli/pkg/platform/client"
	"github.com/codefly-dev/core/wool"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
)

type OrganizationRepository interface {
}

type OrganizationRepo struct {
	client *api.WebAPI
}

func NewPlatformService(ctx context.Context, token string) (*OrganizationRepo, error) {
	w := wool.Get(ctx).In("NewService")

	client, err := NewClient(ctx, token)
	if err != nil {
		return nil, w.Wrap(err)
	}

	version, err := client.OrganizationService.OrganizationServiceVersion(nil)
	if err != nil {
		return nil, w.Wrap(err)
	}
	w.Focus("version", wool.Field("version", version))

	// Call the self API
	self, err := client.OrganizationService.OrganizationServiceGetSelf(nil)
	if err != nil {
		return nil, w.Wrap(err)
	}

	w.Focus("ID", wool.Field("who am I?", self.Payload.User.Name))

	return &OrganizationRepo{client: client}, nil
}

func NewClient(ctx context.Context, token string) (*api.WebAPI, error) {
	// Define the custom endpoint
	customEndpoint, err := url.Parse("http://localhost:21172")
	if err != nil {
		return nil, err
	}
	transport := httptransport.New(customEndpoint.Host, customEndpoint.Path, []string{customEndpoint.Scheme})
	transport.DefaultAuthentication = httptransport.BearerToken(token)

	client := api.New(transport, strfmt.Default)
	return client, nil
}
