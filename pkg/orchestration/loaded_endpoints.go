package orchestration

import (
	"fmt"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/proto"
)

// reconcileLoadedEndpoints keeps the agent's discovered API contract while
// taking reach, addressing and location from the producer's manifest. These
// declarations belong to the composition, as in World.exportableTo, not to a
// runtime's protobuf schema: an older agent cannot report a field it predates.
// No absent declaration is defaulted and no undeclared endpoint is admitted.
//
// Both Load paths run this: Runner.Load for a run, Builder.Load for a build,
// sync, deploy or snapshot. They publish into the same shared state and the same
// network mappings, so reconciling one of them only would make `codefly run` and
// `codefly deploy` disagree about the same manifest and the same agent.
func reconcileLoadedEndpoints(service *resources.Service, reported []*basev0.Endpoint) ([]*basev0.Endpoint, error) {
	if service == nil {
		return nil, fmt.Errorf("runtime endpoints have no producer manifest")
	}
	identity, err := service.Identity()
	if err != nil {
		return nil, err
	}
	declared := make(map[string]*resources.Endpoint, len(service.Endpoints))
	for _, endpoint := range service.Endpoints {
		if endpoint == nil {
			return nil, fmt.Errorf("%s declares a nil endpoint", identity.Unique())
		}
		if _, exists := declared[endpoint.Name]; exists {
			return nil, fmt.Errorf("%s declares endpoint %q twice", identity.Unique(), endpoint.Name)
		}
		declared[endpoint.Name] = endpoint
	}
	seen := make(map[string]bool, len(reported))
	var accepted []*basev0.Endpoint
	for _, endpoint := range reported {
		if endpoint == nil {
			return nil, fmt.Errorf("%s reported a nil endpoint", identity.Unique())
		}
		if endpoint.GetModule() != identity.Module || endpoint.GetService() != identity.Name {
			return nil, fmt.Errorf("runtime endpoint %s does not belong to %s", resources.EndpointDestination(endpoint), identity.Unique())
		}
		manifest, ok := declared[endpoint.GetName()]
		if !ok {
			return nil, fmt.Errorf("runtime endpoint %s is not declared by the producer manifest", resources.EndpointDestination(endpoint))
		}
		if manifest.API != endpoint.GetApi() {
			return nil, fmt.Errorf("runtime endpoint %s reports API %q, but the producer manifest declares %q", resources.EndpointDestination(endpoint), endpoint.GetApi(), manifest.API)
		}
		if seen[endpoint.GetName()] {
			return nil, fmt.Errorf("runtime endpoint %s was reported twice", resources.EndpointDestination(endpoint))
		}
		seen[endpoint.GetName()] = true
		declaration := manifest.Declaration()
		// Unknown wire declarations (including the reserved allow_modules)
		// remain a refusal, rather than disappearing in the reconciliation.
		declaration.UnknownWireFields = resources.EndpointDeclarationOf(endpoint).UnknownWireFields
		if err := resources.ValidateEndpointDeclaration(declaration); err != nil {
			return nil, err
		}
		clone := proto.CloneOf(endpoint)
		clone.Visibility = manifest.Visibility
		clone.Location = manifest.Location
		clone.Exposure = manifest.Exposure
		accepted = append(accepted, clone)
	}
	return accepted, nil
}
