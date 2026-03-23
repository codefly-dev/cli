package imports

import (
	"context"
	"fmt"
	"path"

	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/compose-spec/compose-go/loader"
	"github.com/compose-spec/compose-go/types"
)

func CheckDockerCompose(ctx context.Context, dir string) (*Recommendation, error) {
	w := wool.Get(ctx).In("CheckDockerCompose<%s>", wool.DirField(dir))
	dockerfile := path.Join(dir, "docker-compose.yml")
	exists, err := shared.FileExists(ctx, dockerfile)
	if err != nil {
		return nil, w.Wrapf(err, "cannot check docker-compose.yml")
	}
	if !exists {
		return nil, nil
	}
	config, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: dockerfile}},
	})
	if err != nil {
		return nil, w.Wrapf(err, "cannot load docker-compose.yml")
	}

	// Print parsed services
	for _, service := range config.Services {
		fmt.Println("Name Name:", service.Name)
		fmt.Println("Image:", service.Image)
		// ... and so on for other service properties
	}
	return nil, nil
}
