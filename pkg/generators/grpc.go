package generators

import (
	"context"
	"embed"
	"fmt"
	"os"

	"github.com/codefly-dev/cli/pkg/services/services"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/configurations/languages"
	basev0 "github.com/codefly-dev/core/generated/go/base/v0"
	"github.com/codefly-dev/core/runners"
	"github.com/codefly-dev/core/templates"
	"github.com/codefly-dev/core/version"
	"github.com/codefly-dev/core/wool"
)

func GRPC(ctx context.Context, project *configurations.Project, service *configurations.Service, language languages.Language, destination string) error {
	w := wool.Get(ctx).In("generateGRPCs", wool.ThisField(service))
	endpoints, err := getGRPCEndpoints(ctx, service)
	if err != nil {
		return w.Wrapf(err, "cannot get grpc endpoints")
	}
	err = GenerateGRPC(ctx, language, destination, service.Name, endpoints...)
	if err != nil {
		return w.Wrapf(err, "cannot generate grpc")
	}
	return nil
}

func getGRPCEndpoints(ctx context.Context, service *configurations.Service) ([]*basev0.Endpoint, error) {
	w := wool.Get(ctx).In("getGRPCEndpoints")
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
	// filter-out gRPC
	var endpoints []*basev0.Endpoint
	for _, endpoint := range res.Endpoints {
		if grpc := configurations.IsGRPC(ctx, endpoint.Api); grpc != nil {
			endpoints = append(endpoints, endpoint)
		}
	}
	w.Debug("got endpoints", wool.ProjectField(service.Project), wool.Field("endpoints", configurations.MakeEndpointSummary(endpoints)))
	return endpoints, nil
}

func GenerateGRPC(ctx context.Context, language languages.Language, destination string, service string, endpoints ...*basev0.Endpoint) error {
	w := wool.Get(ctx).In("generateGRPC", wool.Field("destination", destination))
	// call the companion
	image, err := version.CompanionImage(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot get companion image")
	}
	// Create a tmp dir for the proto
	tmpDir, err := os.MkdirTemp("", "proto")
	if err != nil {
		return w.Wrapf(err, "cannot create tmp dir")
	}
	defer func(path string) {
		err := os.RemoveAll(path)
		if err != nil {
			w.Error("cannot remove tmp dir", wool.Field("path", path))
		}
	}(tmpDir)
	destinationVolume := fmt.Sprintf("%s:/workspace/output", destination)
	w.Debug("generating code from buf", wool.DirField(tmpDir), wool.DirField(destination))
	// Buf configuration
	err = CreateBufConfiguration(ctx, tmpDir, service, language)
	if err != nil {
		return w.Wrapf(err, "cannot create buf configuration")
	}
	// Add the proto stuff
	for _, endpoint := range endpoints {
		if grpc := configurations.IsGRPC(ctx, endpoint.Api); grpc != nil {
			unique := fmt.Sprintf("%s_%s", endpoint.Application, endpoint.Service)
			err = os.WriteFile(fmt.Sprintf("%s/%s_%s.proto", tmpDir, unique, endpoint.Name), []byte(grpc.Proto), 0644)
			if err != nil {
				return w.Wrapf(err, "cannot write proto file")
			}
		}
	}
	bufVolume := fmt.Sprintf("%s:/workspace", tmpDir)
	runner, err := runners.NewRunner(ctx, "docker", "run", "--rm",
		"-v", destinationVolume,
		"-v", bufVolume,
		"-w", "/workspace",
		image, "buf", "mod", "update")
	if err != nil {
		return w.Wrapf(err, "can't create runner")
	}
	runner.WithDir(tmpDir)
	err = runner.Run()
	if err != nil {
		return w.Wrapf(err, "cannot generate code from buf")
	}
	runner, err = runners.NewRunner(ctx, "docker", "run", "--rm",
		"-v", destinationVolume,
		"-v", bufVolume,
		"-w", "/workspace",
		image, "buf", "generate")
	if err != nil {
		return w.Wrapf(err, "can't create runner")
	}
	runner.WithDir(tmpDir)
	w.Debug("Generating code from buf", wool.DirField(destination))
	err = runner.Run()
	if err != nil {
		return w.Wrapf(err, "cannot generate code from buf")
	}
	return nil
}

type GoConfiguration struct {
	Destination     string
	GoPackagePrefix string
}

func CreateBufConfiguration(ctx context.Context, bufDir string, service string, language languages.Language) error {
	w := wool.Get(ctx).In("createBufConfiguration")
	switch language {
	case languages.GO:
		// Templatize
		err := templateGoConfiguration(ctx, bufDir, fmt.Sprintf("github.com/codefly-dev/cli/pkg/builder/clients/%s", service))
		if err != nil {
			return w.Wrapf(err, "cannot templatize")
		}
		return nil
	}
	return w.NewError("unknown language")
}

func templateGoConfiguration(ctx context.Context, bufDir string, goPackagePrefix string) error {
	w := wool.Get(ctx).In("templateGoConfiguration", wool.Field("bufDir", bufDir), wool.Field("goPackagePrefix", goPackagePrefix))
	templator := &templates.Templator{NameReplacer: templates.CutTemplateSuffix{}}
	conf := GoConfiguration{
		Destination:     "output",
		GoPackagePrefix: goPackagePrefix,
	}
	err := templator.CopyAndApply(ctx, goFS, "templates/go", bufDir, conf)
	if err != nil {
		return w.Wrapf(err, "cannot copy and apply template")
	}
	return nil
}

// Embed

//go:embed templates/go
var goFS embed.FS
