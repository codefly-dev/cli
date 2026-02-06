package frameworks

import (
	"context"
	"os"
	"path"
	"strings"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/wool"
	"golang.org/x/mod/modfile"
)

func RecommendedGoDependencies(ctx context.Context, dir string) ([]*resources.Agent, error) {
	w := wool.Get(ctx).In("RecommendedGoDependencies", wool.DirField(dir))
	// Parse the go.mod file
	content, err := os.ReadFile(path.Join(dir, "go.mod"))
	if err != nil {
		return nil, w.Wrapf(err, "cannot read go.mod")
	}
	modFile, err := modfile.Parse("go.mod", content, nil)
	if err != nil {
		return nil, w.Wrapf(err, "cannot parse go.mod")
	}

	agents := make(map[resources.Agent]bool)
	for _, require := range modFile.Require {
		p := GuessAgentFromGoRequire(require)
		if p == nil {
			continue
		}
		agents[*p] = true
	}
	var deps []*resources.Agent
	for p := range agents {
		deps = append(deps, &p)
	}
	return deps, nil
}

func GuessAgentFromGoRequire(require *modfile.Require) *resources.Agent {
	if strings.Contains(require.Mod.String(), "redis") {
		return &resources.Agent{
			Publisher: "codefly.ai",
			Name:      "redis",
			Version:   "latest",
			Kind:      resources.ServiceAgent,
		}
	}
	return nil
}
