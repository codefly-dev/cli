package orchestration

import (
	"fmt"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
)

// droppedDeclaredEndpoints names the endpoints a service's manifest declares
// that the agent's Load did not return: endpoints the run can allocate no
// address for, however the manifest declared them.
func droppedDeclaredEndpoints(service *resources.Service, loaded []*basev0.Endpoint) []string {
	if service == nil {
		return nil
	}
	returned := make(map[string]bool, len(loaded))
	for _, endpoint := range loaded {
		if endpoint != nil {
			returned[endpoint.Name] = true
		}
	}
	var dropped []string
	for _, endpoint := range service.Endpoints {
		if endpoint != nil && !returned[endpoint.Name] {
			dropped = append(dropped, endpoint.Name)
		}
	}
	return dropped
}

// loadedAgentName names the agent a service runs under, as its manifest
// declares it, for a message about what that agent answered.
func loadedAgentName(instance *services.Instance) string {
	if instance == nil || instance.Service == nil || instance.Service.Agent == nil {
		return "an agent the manifest does not name"
	}
	agent := instance.Service.Agent
	return fmt.Sprintf("%s/%s:%s", agent.Publisher, agent.Name, agent.Version)
}
