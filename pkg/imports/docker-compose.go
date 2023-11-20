package imports

import (
	"context"
	"fmt"
	"path"

	"github.com/codefly-dev/core/shared"
	"github.com/compose-spec/compose-go/loader"
	"github.com/compose-spec/compose-go/types"
)

func CheckDockerCompose(dir string) (*Recommendation, error) {
	logger := shared.NewLogger("CheckDockerCompose<%s>", dir)
	ctx := context.Background()
	dockerfile := path.Join(dir, "docker-compose.yml")
	if !shared.FileExists(dockerfile) {
		return nil, nil
	}
	config, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: dockerfile}},
	})
	if err != nil {
		return nil, logger.Wrapf(err, "cannot load docker-compose.yml")
	}

	// Print parsed services
	for _, service := range config.Services {
		fmt.Println("Name Name:", service.Name)
		fmt.Println("Image:", service.Image)
		// ... and so on for other service properties
	}
	return nil, nil
}
