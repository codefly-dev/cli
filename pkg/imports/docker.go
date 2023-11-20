package imports

import (
	"fmt"
	"path"

	"github.com/asottile/dockerfile"
	"github.com/codefly-dev/cli/pkg/imports/frameworks"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

func CheckDocker(dir string) (*Recommendation, error) {
	logger := shared.NewLogger("CheckDocker<%s>", dir)
	file := path.Join(dir, "Dockerfile")
	if !shared.FileExists(file) {
		return nil, nil
	}
	// Parse the Dockerfile
	cmds, err := dockerfile.ParseFile(file)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot parse Dockerfile")
	}
	main, err := recommendedMain(cmds)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot recommend bases")
	}
	inc, err := includes(dir, cmds)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot recommend includes")
	}
	dependencies, err := recommendedDependencies(dir, main.Kind)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot recommend dependencies")
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
	Bases []PluginRecommendation
}

func NewGoBase(recs []PluginRecommendation) (*MainServiceRecommendation, error) {
	return &MainServiceRecommendation{
		Kind:  "go",
		Bases: recs,
	}, nil
}

func recommendedMain(cmds []dockerfile.Command) (*MainServiceRecommendation, error) {
	for _, cmd := range cmds {
		if cmd.Cmd == "FROM" {
			return RecommendBaseFromDocker(cmd.Value[0])
		}
	}
	return nil, fmt.Errorf("no FROM command found")
}

func RecommendBaseFromDocker(image string) (*MainServiceRecommendation, error) {
	logger := shared.NewLogger("RecommendBaseFromDocker<%s>", image)
	logger.TODO("IMPLEMENT PYTHON")
	return NewGoBase([]PluginRecommendation{
		{Name: "codefly.ai/go:latest", Description: "Go base image", Reason: "Go is awesome"},
		{Name: "codefly.ai/go-grpc:latest", Description: "Go with gRPC/REST", Reason: "Get a lot more done with less code"},
	})
}

func includes(dir string, cmds []dockerfile.Command) ([]shared.CopyInstruction, error) {
	var results []shared.CopyInstruction
	for _, cmd := range cmds {
		if cmd.Cmd == "COPY" || cmd.Cmd == "ADD" {
			// Only add existing thing
			p := path.Join(dir, cmd.Value[0])
			if !shared.FileExists(p) {
				continue
			}
			results = append(results, shared.CopyInstruction{Name: cmd.Value[0], Path: p})
		}
	}
	return results, nil
}

func recommendedDependencies(dir string, kind string) ([]*configurations.Plugin, error) {
	switch kind {
	case "go":
		return frameworks.RecommendedGoDependencies(dir)
	default:
		return nil, fmt.Errorf("cannot recommend dependencies for %s", kind)
	}
}
