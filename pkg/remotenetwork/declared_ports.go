package remotenetwork

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/codefly-dev/core/resources"
)

// A service manifest may declare its in-cluster ports by hand under
// spec.deployment.endpoint-ports, a map from endpoint name to port. The render
// never reads them — core's network.DeployedEndpointPorts is the allocation —
// so a declaration is only ever a copy another tool keeps, and a copy that
// disagrees is refused before anything is rendered.
const (
	deploymentSpecKey    = "deployment"
	endpointPortsSpecKey = "endpoint-ports"
)

// DeclaredEndpointPorts reads spec.deployment.endpoint-ports from a service
// manifest. It returns nil when nothing is declared, and an error when the
// declaration is not a map of endpoint names to ports.
func DeclaredEndpointPorts(service *resources.Service) (map[string]uint16, error) {
	if service == nil || service.Spec == nil {
		return nil, nil
	}
	rawDeployment, found := service.Spec[deploymentSpecKey]
	if !found || rawDeployment == nil {
		return nil, nil
	}
	deployment, ok := rawDeployment.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("service %s: spec.%s must be a map, got %T", service.Name, deploymentSpecKey, rawDeployment)
	}
	rawPorts, found := deployment[endpointPortsSpecKey]
	if !found || rawPorts == nil {
		return nil, nil
	}
	declared, ok := rawPorts.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("service %s: spec.%s.%s must be a map of endpoint name to port, got %T",
			service.Name, deploymentSpecKey, endpointPortsSpecKey, rawPorts)
	}
	ports := make(map[string]uint16, len(declared))
	for endpoint, value := range declared {
		port, valid := declaredPort(value)
		if !valid {
			return nil, fmt.Errorf("service %s: spec.%s.%s.%s is %v, not a port in 1-65535",
				service.Name, deploymentSpecKey, endpointPortsSpecKey, endpoint, value)
		}
		ports[endpoint] = port
	}
	return ports, nil
}

func declaredPort(value any) (uint16, bool) {
	var port int64
	switch typed := value.(type) {
	case int:
		port = int64(typed)
	case int64:
		port = typed
	case uint64:
		if typed > math.MaxUint16 {
			return 0, false
		}
		port = int64(typed)
	case float64:
		if typed != math.Trunc(typed) || typed < 0 || typed > math.MaxUint16 {
			return 0, false
		}
		port = int64(typed)
	default:
		return 0, false
	}
	if port < 1 || port > math.MaxUint16 {
		return 0, false
	}
	return uint16(port), true
}

// CheckDeclaredEndpointPorts refuses a service whose manifest declares an
// in-cluster port that differs from the one the render allocates, or declares
// one for an endpoint the render does not place in-cluster. allocated is the
// render's allocation for the service (RemoteManager.DeployedPorts). No
// declaration, or a declaration that matches, passes. Every mismatch is
// reported at once.
func CheckDeclaredEndpointPorts(service *resources.Service, allocated map[string]uint16) error {
	declared, err := DeclaredEndpointPorts(service)
	if err != nil || len(declared) == 0 {
		return err
	}
	unique := service.Name
	if identity, identityErr := service.Identity(); identityErr == nil {
		unique = identity.Unique()
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	var problems []error
	for _, name := range names {
		port, placed := allocated[name]
		switch {
		case !placed:
			problems = append(problems, fmt.Errorf(
				"service %s declares spec.%s.%s.%s = %d, but the render places no in-cluster endpoint %q",
				unique, deploymentSpecKey, endpointPortsSpecKey, name, declared[name], name))
		case port != declared[name]:
			problems = append(problems, fmt.Errorf(
				"service %s endpoint %s: declared port %d (spec.%s.%s) differs from the allocated in-cluster port %d; "+
					"read the allocation from `codefly show network --json` or network.DeployedEndpointPorts instead of declaring it",
				unique, name, declared[name], deploymentSpecKey, endpointPortsSpecKey, port))
		}
	}
	return errors.Join(problems...)
}
