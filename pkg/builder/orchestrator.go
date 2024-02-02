package builder

import (
	"context"

	"github.com/codefly-dev/cli/pkg/services/services"
)

var instances map[string]*services.Instance

func init() {
	instances = make(map[string]*services.Instance)
}

func Register(ctx context.Context, instance *services.Instance) {
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
