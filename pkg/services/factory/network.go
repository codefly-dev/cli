package factory

import (
	"github.com/codefly-dev/core/configurations"
	runtimev0 "github.com/codefly-dev/core/generated/go/services/runtime/v0"
)

var networkMappings = map[string][]*runtimev0.NetworkMapping{}

func SetNetworkMappings(unique string, mappings []*runtimev0.NetworkMapping) {
	networkMappings[unique] = mappings
}

func GetNetworkMappingsForService(unique string) []*runtimev0.NetworkMapping {
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
