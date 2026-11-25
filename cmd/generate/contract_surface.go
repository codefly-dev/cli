package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/codefly-dev/core/composition"
	basev1 "github.com/codefly-dev/core/generated/go/codefly/base/v1"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"
)

const contractSurfacesConfigFile = "contracts.codefly.yaml"
const contractSurfacesConfigSchema = "codefly/module-contracts-config/v1"

// Publication configuration belongs to the CLI, like clients.codefly.yaml;
// service.spec belongs to the agent and must not acquire CLI-only settings.
func loadContractSurfacePaths(module *resources.Module) (map[string]string, error) {
	path := filepath.Join(module.Dir(), contractSurfacesConfigFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var config struct {
		Schema   string            `yaml:"schema"`
		Surfaces map[string]string `yaml:"surfaces"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err = decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("%s: %w; see docs/commands.md#generate-contracts", path, err)
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("%s: expected one YAML document", path)
	}
	if config.Schema != contractSurfacesConfigSchema {
		return nil, fmt.Errorf("%s: schema must be %s; see docs/commands.md#generate-contracts", path, contractSurfacesConfigSchema)
	}
	declared := map[string]bool{}
	for _, service := range module.ServiceReferences {
		declared[service.Name] = true
	}
	for service, inventory := range config.Surfaces {
		if !declared[service] || inventory == "" {
			return nil, fmt.Errorf("%s: surfaces[%s] must name a declared service and a service-relative JSON file; see docs/commands.md#generate-contracts", path, service)
		}
	}
	return config.Surfaces, nil
}

// The inventory is projected from its owner's registration/routing tables,
// using Core's published service/procedure vocabulary. The CLI neither imports
// owner code nor interprets an owner's private policy.
type contractSurfaces map[string][]composition.APIContractService

func loadContractSurfaces(service *resources.Service, endpoints []*basev1.Endpoint, path string) (contractSurfaces, error) {
	var protobufEndpoints []string
	for _, endpoint := range endpoints {
		if endpoint.Api == standards.GRPC || endpoint.Api == standards.CONNECT {
			protobufEndpoints = append(protobufEndpoints, endpoint.Name)
		}
	}
	if path == "" {
		if len(protobufEndpoints) > 1 {
			return nil, fmt.Errorf("service %s exports multiple protobuf endpoints %v; configure module file %s with schema: %s and surfaces: {%s: <service-relative JSON file>}; inventory shape is {\"<endpoint>\": [{\"name\": \"Service\", \"fullName\": \"package.Service\", \"procedures\": [\"/package.Service/Method\"]}]}; see docs/commands.md#generate-contracts", service.Name, protobufEndpoints, contractSurfacesConfigFile, contractSurfacesConfigSchema, service.Name)
		}
		return nil, nil
	}
	root, err := os.OpenRoot(service.Dir())
	if err != nil {
		return nil, err
	}
	defer root.Close()
	data, err := root.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("service %s inventory %s (from %s): %w; see docs/commands.md#generate-contracts", service.Name, path, contractSurfacesConfigFile, err)
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
	methods, err := composition.ProtobufProcedures(set, *endpoint)
	if err != nil {
		return nil, fmt.Errorf("endpoint %s/%s: %w", endpoint.Service, endpoint.Endpoint, err)
	}
	return methods, nil
}

// ProtobufCatalogCoverage holds a catalog's protobuf listeners to their
// descriptors the way a module update reads them (core's
// composition.ProtobufCoverage): the listeners of one service that share one
// descriptor serve, between them, every method of every service they list. An
// inventory that omits a method the service registers publishes a catalog in
// which that method is served by no listener, and a composition that calls it
// is refused only when it is rendered; here it is refused where the inventory
// is written. read returns an endpoint's descriptor bytes.
func ProtobufCatalogCoverage(endpoints []composition.APIContractEndpoint, read func(composition.APIContractEndpoint) ([]byte, error)) error {
	type descriptorKey struct{ service, digest string }
	var keys []descriptorKey
	listeners := map[descriptorKey][]composition.APIContractEndpoint{}
	for i := range endpoints {
		endpoint := &endpoints[i]
		if endpoint.Kind != composition.APIContractKindProtobuf {
			continue
		}
		key := descriptorKey{endpoint.Service, endpoint.Digest}
		if _, seen := listeners[key]; !seen {
			keys = append(keys, key)
		}
		listeners[key] = append(listeners[key], *endpoint)
	}
	for _, key := range keys {
		shared := listeners[key]
		data, err := read(shared[0])
		if err != nil {
			return fmt.Errorf("service %s endpoint %s: read its descriptor: %w", key.service, shared[0].Endpoint, err)
		}
		var set descriptorpb.FileDescriptorSet
		if err = googleproto.Unmarshal(data, &set); err != nil {
			return fmt.Errorf("service %s endpoint %s: parse its descriptor: %w", key.service, shared[0].Endpoint, err)
		}
		if err = composition.ProtobufCoverage(&set, shared); err != nil {
			return fmt.Errorf("service %s: %w", key.service, err)
		}
	}
	return nil
}

// publishedInterfacesDir is where a module keeps the definitions of the
// interfaces it publishes: <module>/interfaces/<name>/interface.codefly.yaml.
const publishedInterfacesDir = "interfaces"

// modulePublishedInterfaces resolves an interface identity among the
// definitions the module publishes. Core never fetches a definition; the CLI
// is the host that does, and for an inventory row the definition is the
// module's own, since the row says the module defines the protocol.
func modulePublishedInterfaces(module *resources.Module) resources.InterfaceResolver {
	return func(ctx context.Context, identity *resources.InterfaceIdentity) (*resources.Interface, error) {
		entries, err := os.ReadDir(filepath.Join(module.Dir(), publishedInterfacesDir))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		var published []string
		for _, entry := range entries {
			dir := filepath.Join(module.Dir(), publishedInterfacesDir, entry.Name())
			if _, statErr := os.Stat(filepath.Join(dir, resources.InterfaceConfigurationName)); statErr != nil {
				continue
			}
			definition, loadErr := resources.LoadInterfaceFromDir(ctx, dir)
			if loadErr != nil {
				return nil, loadErr
			}
			if definition.Identity().String() == identity.String() {
				return definition, nil
			}
			published = append(published, definition.Identity().String())
		}
		if len(published) == 0 {
			return nil, fmt.Errorf("module %s publishes no definition of it under %s/", module.Name, publishedInterfacesDir)
		}
		slices.Sort(published)
		return nil, fmt.Errorf("module %s publishes no definition of it under %s/ (it publishes %s)", module.Name, publishedInterfacesDir, strings.Join(published, ", "))
	}
}

// checkInterfaceRows holds every inventory row that declares its service the
// protocol of an interface other modules implement (APIContractService.Interface)
// to the definition it names. Core exempts such a service from being served by
// a listener of the module on the row's word alone, because it reads no
// definition; here the definition is resolved and must list that very service,
// so the exemption cannot be claimed for a service no published interface is
// the protocol of.
func checkInterfaceRows(ctx context.Context, module *resources.Module, surfaces contractSurfaces) error {
	ctx = resources.WithInterfaceResolver(ctx, modulePublishedInterfaces(module))
	endpoints := make([]string, 0, len(surfaces))
	for endpoint := range surfaces {
		endpoints = append(endpoints, endpoint)
	}
	slices.Sort(endpoints)
	for _, endpoint := range endpoints {
		for _, service := range surfaces[endpoint] {
			if service.Interface == "" {
				continue
			}
			declared := fmt.Sprintf("service %s on endpoint %s is declared as interface %s", service.FullName, endpoint, service.Interface)
			identity, err := resources.ParseInterfaceIdentity(service.Interface)
			if err != nil {
				return fmt.Errorf("service %s on endpoint %s: %w", service.FullName, endpoint, err)
			}
			definition, err := resources.ResolveInterface(ctx, identity)
			if err != nil {
				return fmt.Errorf("service %s on endpoint %s: %w", service.FullName, endpoint, err)
			}
			if definition.Protobuf == nil {
				return fmt.Errorf("%s, which is a %s interface and lists no protobuf service", declared, definition.Type)
			}
			var listed []string
			for _, protocol := range definition.Protobuf.Services {
				listed = append(listed, definition.Protobuf.Package+"."+protocol.Name)
			}
			if !slices.Contains(listed, service.FullName) {
				return fmt.Errorf("%s, whose definition lists %s and not it", declared, strings.Join(listed, ", "))
			}
		}
	}
	return nil
}
