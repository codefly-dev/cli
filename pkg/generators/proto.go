package generators

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/version"

	"github.com/codefly-dev/core/builders"
	"github.com/codefly-dev/core/runners"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
)

type Proto struct {
	Dir     string
	version string

	// Keep the proto hash for cashing
	dependencies *builders.Dependencies
}

func NewProto(ctx context.Context, dir string) (*Proto, error) {
	w := wool.Get(ctx).In("proto.NewProto")
	v, err := version.Version(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get v")
	}
	deps := builders.NewDependencies("proto",
		builders.NewDependency(dir).WithPathSelect(shared.NewSelect("*.proto")),
	)
	deps.Localize(dir)

	return &Proto{
		Dir:          dir,
		version:      v,
		dependencies: deps,
	}, nil
}

func (g *Proto) Generate(ctx context.Context) error {
	w := wool.Get(ctx).In("proto.Generate")
	// Check if Docker is running
	if !runners.DockerEngineRunning(ctx) {
		return w.NewError("docker is not running")
	}
	updated, err := g.dependencies.Updated(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot check if updated")
	}
	if !updated {
		w.Debug("no proto change detected")
		return nil
	}
	w.Info("detected changes to the proto: re-generating code", wool.DirField(g.Dir))
	image, err := version.CompanionImage(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot get companion image")
	}
	volume := fmt.Sprintf("%s:/workspace", g.Dir)
	runner, err := runners.NewRunner(ctx, "docker", "run", "--rm", "-v", volume, "-w", "/workspace/proto", image, "buf", "mod", "update")
	if err != nil {
		return w.Wrapf(err, "can't create runner")
	}
	runner.WithDir(g.Dir)
	err = runner.Run()
	if err != nil {
		return w.Wrapf(err, "cannot generate code from buf")
	}
	runner, err = runners.NewRunner(ctx, "docker", "run", "--rm", "-v", volume, "-w", "/workspace/proto", image, "buf", "generate")
	if err != nil {
		return w.Wrapf(err, "can't create runner")
	}
	runner.WithDir(g.Dir)
	w.Debug("Generating code from buf", wool.DirField(g.Dir))
	err = runner.Run()
	if err != nil {
		return w.Wrapf(err, "cannot generate code from buf")
	}
	err = g.dependencies.UpdateCache(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot update cache")
	}
	return nil
}

//func (g *Proto) Valid() bool {
//	runner, err := runners.NewDocker(context.Background())
//	if err != nil {
//		return false
//	}
//	runner.WithWorkDir(path.Join(g.Dir, "proto")
//	runner.WithCommand("buf", "generate")
//	err = runner.Run()
//
//}
