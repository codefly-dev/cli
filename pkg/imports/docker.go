package imports

import (
	"context"
	"fmt"
	"path"

	"github.com/asottile/dockerfile"
	"github.com/codefly-dev/cli/pkg/imports/frameworks"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
)

func CheckDocker(ctx context.Context, dir string) (*Recommendation, error) {
	w := wool.Get(ctx).In("CheckDocker<%s>", wool.DirField(dir))
	file := path.Join(dir, "Dockerfile")

	exists, err := shared.FileExists(ctx, file)
	if err != nil {
		return nil, w.Wrapf(err, "cannot check Dockerfile")
	}
	if !exists {
		return nil, nil
	}
	// Parse the Dockerfile
	cmds, err := dockerfile.ParseFile(file)
	if err != nil {
		return nil, w.Wrapf(err, "cannot parse Dockerfile")
	}
	main, err := recommendedMain(ctx, cmds)
	if err != nil {
		return nil, w.Wrapf(err, "cannot recommend bases")
	}
	inc, err := includes(ctx, dir, cmds)
	if err != nil {
		return nil, w.Wrapf(err, "cannot recommend includes")
	}
	dependencies, err := recommendedDependencies(ctx, dir, main.Kind)
	if err != nil {
		return nil, w.Wrapf(err, "cannot recommend dependencies")
	}
	rec := &Recommendation{
		Main: ServiceRecommendation{
			Names:    main.Bases,
			Includes: inc,
		},
		Dependencies: dependencies,
	}

	return rec, nil
}

type MainServiceRecommendation struct {
	Kind  string // go, etc...
	Bases []AgentRecommendation
}

func NewGoBase(recs []AgentRecommendation) (*MainServiceRecommendation, error) {
	return &MainServiceRecommendation{
		Kind:  "go",
		Bases: recs,
	}, nil
}

func recommendedMain(ctx context.Context, cmds []dockerfile.Command) (*MainServiceRecommendation, error) {
	for _, cmd := range cmds {
		if cmd.Cmd == "FROM" {
			return RecommendBaseFromDocker(ctx, cmd.Value[0])
		}
	}
	return nil, fmt.Errorf("no FROM command found")
}

func RecommendBaseFromDocker(ctx context.Context, image string) (*MainServiceRecommendation, error) {
	return NewGoBase([]AgentRecommendation{
		{Name: "codefly.ai/go:latest", Description: "Go base image", Reason: "Go is awesome"},
		{Name: "codefly.ai/go-grpc:latest", Description: "Go with gRPC/REST", Reason: "Get a lot more done with less code"},
	})
}

func includes(ctx context.Context, dir string, cmds []dockerfile.Command) ([]shared.CopyInstruction, error) {
	var results []shared.CopyInstruction
	for _, cmd := range cmds {
		if cmd.Cmd == "COPY" || cmd.Cmd == "ADD" {
			// Only add existing thing
			p := path.Join(dir, cmd.Value[0])
			exists, err := shared.FileExists(ctx, p)
			if err != nil {
				return nil, err
			}
			if !exists {
				continue
			}
			results = append(results, shared.CopyInstruction{Name: cmd.Value[0], Path: p})
		}
	}
	return results, nil
}

func recommendedDependencies(ctx context.Context, dir string, kind string) ([]*resources.Agent, error) {
	switch kind {
	case "go":
		return frameworks.RecommendedGoDependencies(ctx, dir)
	default:
		return nil, fmt.Errorf("cannot recommend dependencies for %s", kind)
	}
}
