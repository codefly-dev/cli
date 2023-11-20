package imports

import (
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

type ServiceRecommendation struct {
	Names    []PluginRecommendation
	Includes []shared.CopyInstruction
}

type Recommendation struct {
	Main         ServiceRecommendation
	Dependencies []*configurations.Plugin
	Name         string
}

type PluginRecommendation struct {
	Name        string
	Description string
	Reason      string
}
