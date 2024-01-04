package factory

import (
	"github.com/codefly-dev/core/configurations"
	runtimev1 "github.com/codefly-dev/core/generated/go/services/runtime/v1"
)

var networkMappings = map[string][]*runtimev1.NetworkMapping{}

func SetNetworkMappings(unique string, mappings []*runtimev1.NetworkMapping) {
	networkMappings[unique] = mappings
}

func GetNetworkMappingsForService(unique string) []*runtimev1.NetworkMapping {
	return networkMappings[unique]
}

func GetAddressesForEndpoint(application string, service string, endpoint string) []string {
	unique := configurations.ServiceUnique(application, service)
	nm := networkMappings[unique]
	var addresses []string
	for _, mapping := range nm {
		if mapping.Endpoint.Name == endpoint {
			addresses = append(addresses, mapping.Addresses...)
		}
	}
	return addresses
}
