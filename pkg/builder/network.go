package builder

import (
	"github.com/codefly-dev/core/configurations"
)

var networkMappings = map[string][]*basev0.NetworkMapping{}

func SetNetworkMappings(unique string, mappings []*basev0.NetworkMapping) {
	networkMappings[unique] = mappings
}

func GetNetworkMappingsForService(unique string) []*basev0.NetworkMapping {
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
