package frameworks

import (
	"os"
	"path"
	"strings"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"golang.org/x/mod/modfile"
)

func RecommendedGoDependencies(dir string) ([]*configurations.Agent, error) {
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

	agents := make(map[configurations.Agent]bool)
	for _, require := range modFile.Require {
		p := GuessAgentFromGoRequire(require)
		if p == nil {
			continue
		}
		agents[*p] = true
	}
	var deps []*configurations.Agent
	for p := range agents {
		deps = append(deps, &p)
	}
	return deps, nil
}

func GuessAgentFromGoRequire(require *modfile.Require) *configurations.Agent {
	if strings.Contains(require.Mod.String(), "redis") {
		return &configurations.Agent{
			Publisher:  "codefly.ai",
			Identifier: "redis",
			Version:    "latest",
			Kind:       configurations.AgentService,
		}
	}
	return nil
}
