package frameworks

import (
	"os"
	"path"
	"strings"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"golang.org/x/mod/modfile"
)

func RecommendedGoDependencies(dir string) ([]*configurations.Plugin, error) {
	logger := shared.NewLogger("RecommendedGoDependencies<%s>", dir)
	// Parse the go.mod file
	content, err := os.ReadFile(path.Join(dir, "go.mod"))
	if err != nil {
		return nil, logger.Wrapf(err, "cannot read go.mod")
	}
	modFile, err := modfile.Parse("go.mod", content, nil)
	if err != nil {
		return nil, logger.Wrapf(err, "cannot parse go.mod")
	}

	plugins := make(map[configurations.Plugin]bool)
	for _, require := range modFile.Require {
		p := GuessPluginFromGoRequire(require)
		if p == nil {
			continue
		}
		plugins[*p] = true
	}
	var deps []*configurations.Plugin
	for p := range plugins {
		deps = append(deps, &p)
	}
	return deps, nil
}

func GuessPluginFromGoRequire(require *modfile.Require) *configurations.Plugin {
	if strings.Contains(require.Mod.String(), "redis") {
		return &configurations.Plugin{
			Publisher:  "codefly.ai",
			Identifier: "redis",
			Version:    "latest",
			Kind:       configurations.PluginService,
		}
	}
	return nil
}
