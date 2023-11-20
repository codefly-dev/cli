package network

import (
	"github.com/codefly-dev/cli/pkg/plugins/endpoints"
	runtimev1 "github.com/codefly-dev/cli/proto/v1/services/runtime"
	"github.com/codefly-dev/core/configurations"
)

// ConvertToEnvironmentVariables converts NetworkMapping to environment variables
func ConvertToEnvironmentVariables(nets []*runtimev1.NetworkMapping) ([]string, error) {
	var envs []string
	for _, net := range nets {
		e, err := endpoints.FromProtoEndpoint(net.Endpoint)
		if err != nil {
			return nil, err
		}
		envs = append(envs, configurations.AsEndpointEnvironmentVariable(net.Application, net.Service, e, net.Addresses))
	}
	return envs, nil
}
