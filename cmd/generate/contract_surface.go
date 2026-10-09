package generate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"google.golang.org/protobuf/types/descriptorpb"
)

const apiContractSurfacesSetting = "api-contract-surfaces"

// The owner generates this inventory from the same registration/routing tables
// its listeners use. Reuse Core's published service/procedure vocabulary; the
// CLI neither imports owner code nor interprets an owner's private policy.
type contractSurfaces map[string][]composition.APIContractService

func loadContractSurfaces(service *resources.Service, endpoints []*basev0.Endpoint) (contractSurfaces, error) {
	var protobufEndpoints []string
	for _, endpoint := range endpoints {
		if endpoint.Api == standards.GRPC || endpoint.Api == standards.CONNECT {
			protobufEndpoints = append(protobufEndpoints, endpoint.Name)
		}
	}
	setting, configured := service.Spec[apiContractSurfacesSetting]
	if !configured {
		if len(protobufEndpoints) > 1 {
			return nil, fmt.Errorf("service %s exports multiple protobuf endpoints %v; set spec.%s to the generated endpoint procedure inventory instead of assuming every listener serves the full descriptor", service.Name, protobufEndpoints, apiContractSurfacesSetting)
		}
		return nil, nil
	}
	path, ok := setting.(string)
	if !ok || path == "" {
		return nil, fmt.Errorf("service %s spec.%s must name a generated JSON file relative to the service directory", service.Name, apiContractSurfacesSetting)
	}
	root, err := os.OpenRoot(service.Dir())
	if err != nil {
		return nil, err
	}
	defer root.Close()
	data, err := root.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("service %s spec.%s: %w", service.Name, apiContractSurfacesSetting, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var surfaces contractSurfaces
	if err = decoder.Decode(&surfaces); err != nil {
		return nil, fmt.Errorf("service %s contract surfaces %s: %w", service.Name, path, err)
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("service %s contract surfaces %s: expected one JSON document", service.Name, path)
	}
	declared := map[string]bool{}
	for _, endpoint := range service.Endpoints {
		declared[endpoint.Name] = true
	}
	for endpoint := range surfaces {
		if !declared[endpoint] {
			return nil, fmt.Errorf("service %s contract surfaces %s names undeclared endpoint %s", service.Name, path, endpoint)
		}
	}
	for _, endpoint := range protobufEndpoints {
		if len(surfaces[endpoint]) == 0 {
			return nil, fmt.Errorf("service %s contract surfaces %s has no service inventory for endpoint %s", service.Name, path, endpoint)
		}
	}
	return surfaces, nil
}

// endpointProcedures validates the endpoint's served surface against its
// descriptor. A descriptor supplies schemas and options, including transitive
// imports; the catalog says which of those methods this listener serves.
func endpointProcedures(set *descriptorpb.FileDescriptorSet, endpoint *composition.APIContractEndpoint) ([]string, error) {
	available := map[string]map[string]bool{}
	names := map[string]string{}
	for _, service := range composition.ProtobufServices(set, endpoint.Package) {
		methods := map[string]bool{}
		for _, method := range service.Procedures {
			methods[method] = true
		}
		available[service.FullName] = methods
		names[service.FullName] = service.Name
	}
	seen := map[string]bool{}
	seenServices := map[string]bool{}
	var methods []string
	for _, service := range endpoint.Services {
		declared, exists := available[service.FullName]
		if !exists {
			return nil, fmt.Errorf("endpoint %s/%s: service %s is absent from its descriptor package %s", endpoint.Service, endpoint.Endpoint, service.FullName, endpoint.Package)
		}
		if service.Name != names[service.FullName] || seenServices[service.FullName] {
			return nil, fmt.Errorf("endpoint %s/%s: service %s must use descriptor name %s and appear exactly once", endpoint.Service, endpoint.Endpoint, service.FullName, names[service.FullName])
		}
		seenServices[service.FullName] = true
		for _, method := range service.Procedures {
			if !declared[method] {
				return nil, fmt.Errorf("endpoint %s/%s: procedure %s is absent from its descriptor service %s", endpoint.Service, endpoint.Endpoint, method, service.FullName)
			}
			if seen[method] {
				return nil, fmt.Errorf("endpoint %s/%s declares procedure twice: %s", endpoint.Service, endpoint.Endpoint, method)
			}
			seen[method] = true
			methods = append(methods, method)
		}
	}
	sort.Strings(methods)
	return methods, nil
}
