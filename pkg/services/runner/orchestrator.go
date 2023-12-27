package runner

import (
	"context"

	"github.com/codefly-dev/core/agents/services"
)

var instances map[string]*services.ServiceInstance

func init() {
	instances = make(map[string]*services.ServiceInstance)
}

func Register(ctx context.Context, instance *services.ServiceInstance) {
	instances[instance.Service.Unique()] = instance
}

type AgentProcessInfo struct {
	Application string
	Service     string
	AgentPID    int
}

func AgentPIDs(ctx context.Context) ([]*AgentProcessInfo, error) {
	var infos []*AgentProcessInfo
	for _, instance := range instances {
		infos = append(infos, &AgentProcessInfo{
			Application: instance.Application,
			Service:     instance.Name,
			AgentPID:    instance.ProcessInfo.AgentPID,
		})
	}
	return infos, nil
}
