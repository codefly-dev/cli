package generators

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/cli/pkg/services/services"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/configurations/languages"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/runners"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/version"
	"github.com/codefly-dev/core/wool"
)

func OpenAPI(ctx context.Context, project *configurations.Project, service *configurations.Service, language languages.Language, destination string) error {
	w := wool.Get(ctx).In("generateOpenAPIs", wool.ThisField(service))
	endpoints, err := getOpenAPIEndpoints(ctx, service)
	if err != nil {
		return w.Wrapf(err, "cannot get OpenAPI endpoints")
	}
	err = GenerateOpenAPI(ctx, language, destination, service.Name, endpoints...)
	if err != nil {
		return w.Wrapf(err, "cannot generate OpenAPI")
	}
	return nil
}

func getOpenAPIEndpoints(ctx context.Context, service *configurations.Service) ([]*basev0.Endpoint, error) {
	w := wool.Get(ctx).In("getOpenAPIEndpoints")
	// Use the Builder
	instance, err := services.Load(ctx, service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load builder")
	}
	err = instance.LoadBuilder(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load builder")
	}
	res, err := instance.Builder.Load(ctx, configurations.LocalProto())
	if err != nil {
		return nil, w.Wrapf(err, "cannot load builder")
	}
	// filter-out OpenAPI
	var endpoints []*basev0.Endpoint
	for _, endpoint := range res.Endpoints {
		if rest := configurations.IsRest(ctx, endpoint.Api); rest != nil {
			endpoints = append(endpoints, endpoint)
		}
	}
	w.Debug("got endpoints", wool.ProjectField(service.Project), wool.Field("endpoints", configurations.MakeEndpointSummary(endpoints)))
	return endpoints, nil
}

func GenerateOpenAPI(ctx context.Context, language languages.Language, destination string, service string, endpoints ...*basev0.Endpoint) error {
	w := wool.Get(ctx).In("generateOpenAPI", wool.Field("destination", destination))
	// call the companion
	_, err := shared.CheckDirectoryOrCreate(ctx, destination)

	image, err := version.CompanionImage(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot get companion image")
	}
	// Create a tmp dir for the proto
	swaggerDir, err := os.MkdirTemp("", "openapi")
	if err != nil {
		return w.Wrapf(err, "cannot create tmp dir")
	}
	defer func(path string) {
		err := os.RemoveAll(path)
		if err != nil {
			w.Error("cannot remove tmp dir", wool.Field("path", path))
		}
	}(swaggerDir)

	// Add the proto stuff
	var files []string
	var file string
	var unique string
	for _, endpoint := range endpoints {
		if rest := configurations.IsRest(ctx, endpoint.Api); rest != nil {
			unique = fmt.Sprintf("%s_%s", endpoint.Application, endpoint.Service)
			file = fmt.Sprintf("%s_%s.rest", unique, endpoint.Name)
			files = append(files, file)
			err = os.WriteFile(filepath.Join(swaggerDir, file), rest.Openapi, 0644)
			if err != nil {
				return w.Wrapf(err, "cannot write open api file")
			}
		}
	}
	w.Debug("generating code", wool.SliceCountField(files), wool.DirField(destination))
	if len(files) == 0 {
		return w.NewError("no files to generate")
	}
	if len(files) > 1 {
		return w.NewError("cannot generate code from multiple files")
	}
	switch language {
	case languages.GO:
		return generateOpenAPIGo(ctx, unique, image, destination, swaggerDir, file)
	default:
		return w.Wrapf(err, "language not supported")
	}
}

func generateOpenAPIGo(ctx context.Context, unique string, image string, destinationDir string, swaggerDir, file string) error {
	w := wool.Get(ctx).In("generateOpenAPIGo", wool.Field("destinationDir", destinationDir))
	w.Info("generating openapi go client", wool.Field("file", file))
	// We need to go back to the root to find the go mod and mount this as a volume

	root, err := findModRoot(destinationDir)
	if err != nil {
		return w.Wrapf(err, "cannot find go.mod")
	}
	target, err := filepath.Rel(root, destinationDir)
	if err != nil {
		return w.Wrapf(err, "cannot find relative path")
	}

	fileVolume := fmt.Sprintf("%s:/workspace/swagger", swaggerDir)
	rootVolume := fmt.Sprintf("%s:/workspace", root)

	swaggerFile := filepath.Join("/workspace/swagger", file)

	runner, err := runners.NewRunner(ctx, "docker", "run", "--rm",
		"-v", rootVolume,
		"-v", fileVolume,
		"-w", "/workspace",
		image,
		"swagger",
		"generate",
		"client",
		"-f",
		swaggerFile,
		"-A",
		unique,
		"--target",
		target,
	)
	if err != nil {
		return w.Wrapf(err, "can't create runner")
	}
	runner.WithDir(root)
	err = runner.Run()
	if err != nil {
		return w.Wrapf(err, "cannot generate code from buf")
	}
	return nil
}

func findModRoot(destination string) (string, error) {
	root := destination
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			return root, nil
		}
		root = filepath.Dir(root)
		if root == "/" || root == "." {
			return "", fmt.Errorf("cannot find go.mod")
		}
	}
}
