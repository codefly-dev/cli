package platform

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
)

func LoadToken(ctx context.Context, workspace *configurations.Workspace) (string, error) {
	// Load from environment variable first
	token := os.Getenv("CODEFLY_TOKEN")
	if token != "" {
		return token, nil
	}
	// Load from file
	tokenBytes, err := os.ReadFile(filepath.Join(workspace.Dir(), ".token"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(tokenBytes)), nil
}

func Login(ctx context.Context, token string) error {
	w := wool.Get(ctx).In("login")
	client, err := NewPlatformService(ctx, token)
	if err != nil {
		return w.Wrapf(err, "cannot create platform service")
	}
	version, err := client.API.OrganizationService.OrganizationServiceVersion(nil)
	if err != nil {
		return w.Wrapf(err, "cannot get version")
	}
	w.Debug("version", wool.Field("version", version.Payload.Version))

	//// Call the self API
	//self, err := client.API.OrganizationService.OrganizationServiceGetSelf(nil)
	//if err != nil {
	//	return w.Wrap(err)
	//}
	//w.Debug("ID", wool.Field("who am I?", self.Payload.User.Name))

	return err
}
