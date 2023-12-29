package runner

import runtimev1 "github.com/codefly-dev/core/generated/go/services/runtime/v1"

var networkMappings = map[string][]*runtimev1.NetworkMapping{}

func SetNetworkMappings(unique string, mappings []*runtimev1.NetworkMapping) {
	networkMappings[unique] = mappings
}

func GetNetworkMappingsFor(unique string) []*runtimev1.NetworkMapping {
	return networkMappings[unique]
}

func GetNetworkMappings() []*runtimev1.NetworkMapping {
	var mappings []*runtimev1.NetworkMapping
	for _, v := range networkMappings {
		mappings = append(mappings, v...)
	}
	return mappings
}
