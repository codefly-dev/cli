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
	scratchVolumesKey = "scratch-volumes"
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
	// ScratchVolumes are the scratch volumes this service's agent renders and
	// mounts: writable space for a read-only root filesystem, which the platform
	// owns. A deployed workload may carry exactly these and nothing else.
	//
	// They are declared rather than recognised because there is no property of a
	// volume that distinguishes the platform's scratch directory from a second
	// emptyDir mounted over /secrets to deliver credentials: both are an emptyDir.
	// What the platform renders, the platform declares.
	ScratchVolumes []ScratchVolume
}

// ScratchVolume is one declared scratch volume: the name the agent renders it
// under and the single path it mounts it at.
type ScratchVolume struct {
	Name  string `yaml:"name"`
	Mount string `yaml:"mount"`
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
func (contracts Contracts) Add(contract *ServiceContract) {
	contracts[Subject{Module: contract.Module, Service: contract.Service}.String()] = *contract
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
		Storage        string          `yaml:"storage"`
		ScratchVolumes []ScratchVolume `yaml:"scratch-volumes"`
	}
	if err := yaml.Unmarshal(encoded, &spec); err != nil {
		return ServiceContract{}, fmt.Errorf("service %s: spec.%s must declare %s as a string and %s as a list of {name, mount}: %w",
			service.Name, deploymentSpecKey, storageKey, scratchVolumesKey, err)
	}
	for index, volume := range spec.ScratchVolumes {
		if strings.TrimSpace(volume.Name) == "" || strings.TrimSpace(volume.Mount) == "" {
			return ServiceContract{}, fmt.Errorf(
				"service %s: spec.%s.%s[%d] must declare both name and mount: a scratch volume is a name AND the one path it is mounted at",
				service.Name, deploymentSpecKey, scratchVolumesKey, index)
		}
	}
	contract.ScratchVolumes = spec.ScratchVolumes
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
func (contract *ServiceContract) declaredTLSEndpoints() []EndpointContract {
	var declared []EndpointContract
	for _, endpoint := range contract.Endpoints {
		if tlsProtocols[strings.ToLower(endpoint.API)] || tlsProtocols[strings.ToLower(endpoint.Name)] {
			declared = append(declared, endpoint)
		}
	}
	return declared
}

// scratchVolume reports the declaration for a volume name, if the service
// declared one.
func (contract *ServiceContract) scratchVolume(name string) (ScratchVolume, bool) {
	for _, volume := range contract.ScratchVolumes {
		if volume.Name == name {
			return volume, true
		}
	}
	return ScratchVolume{}, false
}

// declaredScratch is how a refusal names what the service did declare, so the
// reader can see what was expected rather than only what was refused.
func (contract *ServiceContract) declaredScratch() string {
	if len(contract.ScratchVolumes) == 0 {
		return "none"
	}
	declared := make([]string, 0, len(contract.ScratchVolumes))
	for _, volume := range contract.ScratchVolumes {
		declared = append(declared, fmt.Sprintf("%s at %s", volume.Name, volume.Mount))
	}
	return strings.Join(declared, ", ")
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
