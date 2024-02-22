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
	_, err := NewPlatformService(ctx, token)
	if err != nil {
		return w.Wrapf(err, "cannot create platform service")
	}
	return err
}
