package imports

import (
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

type ServiceRecommendation struct {
	Names    []AgentRecommendation
	Includes []shared.CopyInstruction
}

type Recommendation struct {
	Main         ServiceRecommendation
	Dependencies []*configurations.Agent
	Name         string
}

type AgentRecommendation struct {
	Name        string
	Description string
	Reason      string
}
