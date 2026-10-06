package posture

import (
	"fmt"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// The storage modes a service may declare for itself. A deployed render admits
// only the durable one, and admits NEITHER by default: a workload that declares
// nothing is refused by name (see checkDeclarationsArePresent). Absence is not
// conformance — that is the invariant this file exists to hold.
const (
	StorageModeDurable   = "durable"
	StorageModeEphemeral = "ephemeral"
)

// The transports a service may declare for itself: the platform's, or its own TLS.
const (
	TransportMesh   = "mesh"
	TransportOwnTLS = "own-tls"
)

// deploymentSpecKey and the keys under it locate a service's declarations:
// spec.deployment.{storage,transport,scratch-volumes}.
const (
	deploymentSpecKey = "deployment"
	storageKey        = "storage"
	transportKey      = "transport"
	scratchVolumesKey = "scratch-volumes"
)

// ServiceContract is what a service declares about itself. It is the authority
// for the two questions a render cannot answer from manifests — what a process
// does with its state, and whether it terminates its own transport — and the
// rendered configuration is then checked AGAINST it (see configuration.go). A
// declaration with no agreement check authorizes whatever contradicts it; an
// inspection with no declaration can be defeated by a spelling. Both halves, or
// neither works.
type ServiceContract struct {
	Module  string
	Service string
	// StorageMode is spec.deployment.storage. Empty means the service declared
	// nothing, which a deployed render refuses by name.
	StorageMode string
	// Transport is spec.deployment.transport. Empty means the service declared
	// nothing, which a deployed render refuses by name.
	Transport string
	// ScratchVolumes are the scratch volumes this service's agent renders and
	// mounts. A deployed workload carries exactly these.
	ScratchVolumes []ScratchVolume
	// Endpoints are the service's own declared endpoints, carrying the protocol
	// and whether the endpoint lives outside the cell.
	Endpoints []EndpointContract
}

// ScratchVolume is one declared scratch volume: the name the agent renders it
// under and the single path it mounts it at.
type ScratchVolume struct {
	Name  string `yaml:"name"`
	Mount string `yaml:"mount"`
}

// EndpointContract is one declared endpoint. Protocol is the endpoint's declared
// API and nothing else: an endpoint may be NAMED "https" while declaring the
// `http` protocol, and the declaration is what the service serves.
type EndpointContract struct {
	Name     string
	Protocol string
	// External marks an endpoint that lives outside the cell, so it is not
	// transport between in-cell peers at all.
	External bool
}

// Contracts are the service contracts of one render, keyed as a Subject prints.
type Contracts map[string]ServiceContract

// Of returns the contract declared for a subject, and whether there is one.
func (contracts Contracts) Of(subject Subject) (ServiceContract, bool) {
	contract, declared := contracts[subject.String()]
	return contract, declared
}

// Add records one service's contract.
func (contracts Contracts) Add(contract *ServiceContract) {
	contracts[Subject{Module: contract.Module, Service: contract.Service}.String()] = *contract
}

// ContractFromService reads what a service declares about itself. Every render
// path reads declarations through this one function, and every path must
// propagate its error: a contract that failed to parse is not an absent contract,
// it is a refusal (pattern 3 of the audit's lessons — one authority, every
// caller).
func ContractFromService(module string, service *resources.Service) (ServiceContract, error) {
	contract := ServiceContract{Module: module, Service: service.Name}
	for _, endpoint := range service.Endpoints {
		if endpoint == nil {
			continue
		}
		contract.Endpoints = append(contract.Endpoints, EndpointContract{
			Name:     endpoint.Name,
			Protocol: endpoint.API,
			External: endpoint.Location == resources.LocationExternal,
		})
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
		Transport      string          `yaml:"transport"`
		ScratchVolumes []ScratchVolume `yaml:"scratch-volumes"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(encoded)))
	if err := decoder.Decode(&spec); err != nil {
		return ServiceContract{}, fmt.Errorf(
			"service %s: spec.%s must declare %s and %s as strings and %s as a list of {name, mount}: %w",
			service.Name, deploymentSpecKey, storageKey, transportKey, scratchVolumesKey, err)
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
	transport := strings.ToLower(strings.TrimSpace(spec.Transport))
	switch transport {
	case "", TransportMesh, TransportOwnTLS:
		contract.Transport = transport
	default:
		return ServiceContract{}, fmt.Errorf(
			"service %s: spec.%s.%s = %q is not a transport (%s or %s)",
			service.Name, deploymentSpecKey, transportKey, spec.Transport, TransportMesh, TransportOwnTLS)
	}
	for index, volume := range spec.ScratchVolumes {
		if strings.TrimSpace(volume.Name) == "" || strings.TrimSpace(volume.Mount) == "" {
			return ServiceContract{}, fmt.Errorf(
				"service %s: spec.%s.%s[%d] must declare both name and mount: a scratch volume is a name AND the one path it is mounted at",
				service.Name, deploymentSpecKey, scratchVolumesKey, index)
		}
	}
	contract.ScratchVolumes = spec.ScratchVolumes
	return contract, nil
}

// tlsProtocols are the endpoint protocols a service terminates itself.
var tlsProtocols = map[string]bool{
	"https": true, "grpcs": true, "tls": true, "mtls": true, "ssl": true, "wss": true,
}

// declaredTLSEndpoints lists the IN-CELL endpoints whose declared protocol is TLS
// the service terminates itself. An endpoint declared external is somewhere else's
// transport and is not one; the endpoint's NAME is never read as a protocol.
func (contract *ServiceContract) declaredTLSEndpoints() []EndpointContract {
	var declared []EndpointContract
	for _, endpoint := range contract.Endpoints {
		if endpoint.External {
			continue
		}
		if tlsProtocols[strings.ToLower(strings.TrimSpace(endpoint.Protocol))] {
			declared = append(declared, endpoint)
		}
	}
	return declared
}

// scratchVolume reports the declaration for a volume name, if there is one.
func (contract *ServiceContract) scratchVolume(name string) (ScratchVolume, bool) {
	for _, volume := range contract.ScratchVolumes {
		if volume.Name == name {
			return volume, true
		}
	}
	return ScratchVolume{}, false
}

// declaredScratch is how a refusal names what the service did declare.
func (contract *ServiceContract) declaredScratch() string {
	if len(contract.ScratchVolumes) == 0 {
		return valueNone
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
