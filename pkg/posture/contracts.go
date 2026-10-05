package posture

import (
	"fmt"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// StorageModeDurable and StorageModeEphemeral are the storage modes a service may
// declare for itself. A deployed render admits only the durable one.
const (
	StorageModeDurable   = "durable"
	StorageModeEphemeral = "ephemeral"
)

// deploymentSpecKey and storageKey locate the declaration in a service's own
// spec: spec.deployment.storage.
const (
	deploymentSpecKey = "deployment"
	storageKey        = "storage"
)

// ServiceContract is what a service declares about itself that the deployed
// posture is decided by. It is the whole reason this guard is not a reader of
// container command lines: what a process does with its state or its transport
// cannot be proven from a manifest, so the service says it, and the render holds
// the manifests to what was said.
type ServiceContract struct {
	Module  string
	Service string
	// StorageMode is spec.deployment.storage: "durable" or "ephemeral", empty when
	// the service declares nothing.
	StorageMode string
	// Endpoints are the service's own declared endpoints, which is where a
	// transport protocol is declared if there is one.
	Endpoints []EndpointContract
}

// EndpointContract is one declared endpoint of a service.
type EndpointContract struct {
	Name string
	API  string
}

// Contracts are the service contracts of one render, keyed as a Subject prints.
type Contracts map[string]ServiceContract

// Of returns the contract declared for a subject, and whether there is one. A
// unit with no contract has declared nothing, which is not the same as having
// declared conformance — see checkStorageDeclaration.
func (contracts Contracts) Of(subject Subject) (ServiceContract, bool) {
	contract, declared := contracts[subject.String()]
	return contract, declared
}

// Add records one service's contract.
func (contracts Contracts) Add(contract ServiceContract) {
	contracts[Subject{Module: contract.Module, Service: contract.Service}.String()] = contract
}

// ContractFromService reads what a service declares about itself. It is the one
// reader of that declaration, so every render path sees the same contract.
func ContractFromService(module string, service *resources.Service) (ServiceContract, error) {
	contract := ServiceContract{Module: module, Service: service.Name}
	for _, endpoint := range service.Endpoints {
		if endpoint == nil {
			continue
		}
		contract.Endpoints = append(contract.Endpoints, EndpointContract{Name: endpoint.Name, API: endpoint.API})
	}
	declared, present := service.Spec[deploymentSpecKey]
	if !present || declared == nil {
		return contract, nil
	}
	encoded, err := yaml.Marshal(declared)
	if err != nil {
		return ServiceContract{}, fmt.Errorf("service %s: encode spec.%s: %w", service.Name, deploymentSpecKey, err)
	}
	var spec struct {
		Storage string `yaml:"storage"`
	}
	if err := yaml.Unmarshal(encoded, &spec); err != nil {
		return ServiceContract{}, fmt.Errorf("service %s: spec.%s.%s must be a string: %w",
			service.Name, deploymentSpecKey, storageKey, err)
	}
	mode := strings.ToLower(strings.TrimSpace(spec.Storage))
	switch mode {
	case "", StorageModeDurable, StorageModeEphemeral:
		contract.StorageMode = mode
	default:
		return ServiceContract{}, fmt.Errorf(
			"service %s: spec.%s.%s = %q is not a storage mode (%s or %s)",
			service.Name, deploymentSpecKey, storageKey, spec.Storage, StorageModeDurable, StorageModeEphemeral)
	}
	return contract, nil
}

// tlsProtocols are the endpoint protocols that put transport in the service's
// own hands. An endpoint declared as one of these on a mesh-protected
// environment is the declaration the posture refuses; it is read from what the
// service declares, never guessed from how its container is started.
var tlsProtocols = map[string]bool{
	"https": true, "grpcs": true, "tls": true, "mtls": true, "ssl": true, "wss": true,
}

// declaredTLSEndpoints lists the endpoints whose declared protocol is TLS the
// service terminates itself, in declaration order.
func (contract ServiceContract) declaredTLSEndpoints() []EndpointContract {
	var declared []EndpointContract
	for _, endpoint := range contract.Endpoints {
		if tlsProtocols[strings.ToLower(endpoint.API)] || tlsProtocols[strings.ToLower(endpoint.Name)] {
			declared = append(declared, endpoint)
		}
	}
	return declared
}

// sortedSubjects lists the subjects of a contract set, for deterministic output.
func (contracts Contracts) sortedSubjects() []string {
	subjects := make([]string, 0, len(contracts))
	for subject := range contracts {
		subjects = append(subjects, subject)
	}
	sort.Strings(subjects)
	return subjects
}
