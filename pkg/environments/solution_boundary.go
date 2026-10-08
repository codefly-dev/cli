package environments

import (
	"fmt"
	"strings"
)

// EnvironmentSolutionBoundary declares what a composed solution may reach
// directly in this environment. It exists because the alternative is inference:
// nothing in a module's contract says which of its services is the door a guest
// arrives through, so a render that is not told has to either guess or admit
// everything. It is told here.
type EnvironmentSolutionBoundary struct {
	// HostSurface is every service, or endpoint of a service, a composed
	// solution may open a connection to directly. Absent or empty permits
	// nothing: a composition that has not said what a guest may reach has not
	// said that it may reach anything.
	HostSurface []EnvironmentHostSurfaceEntry `yaml:"host-surface,omitempty"`
}

// EnvironmentHostSurfaceEntry is one permitted service or endpoint.
type EnvironmentHostSurfaceEntry struct {
	// Service is the module-qualified "<module>/<service>". A bare service name
	// is refused: two composed modules routinely ship a service of the same
	// name, and a permission that matches both is not the permission anyone
	// wrote.
	Service string `yaml:"service"`
	// Endpoint, when set, permits only that endpoint. Absent permits the
	// service's endpoints, which is what a declaration naming a whole door
	// means.
	Endpoint string `yaml:"endpoint,omitempty"`
}

// Module and Name split the qualified service of a validated entry.
func (entry EnvironmentHostSurfaceEntry) Module() string { module, _ := entry.split(); return module }
func (entry EnvironmentHostSurfaceEntry) Name() string   { _, name := entry.split(); return name }

func (entry EnvironmentHostSurfaceEntry) split() (string, string) {
	module, name, found := strings.Cut(strings.TrimSpace(entry.Service), "/")
	if !found {
		return "", strings.TrimSpace(entry.Service)
	}
	return module, name
}

// validate refuses a declaration that does not name one exact service.
func (boundary *EnvironmentSolutionBoundary) validate() error {
	if boundary == nil {
		return nil
	}
	seen := map[string]bool{}
	for index, entry := range boundary.HostSurface {
		module, name := entry.split()
		if module == "" || name == "" || strings.Contains(name, "/") {
			return fmt.Errorf(
				"solution-boundary.host-surface entry %d must name one service as \"<module>/<service>\"; a bare or malformed name matches whichever module happens to ship it",
				index+1)
		}
		key := module + "/" + name + "/" + strings.TrimSpace(entry.Endpoint)
		if seen[key] {
			return fmt.Errorf("solution-boundary.host-surface names %s twice", strings.TrimSuffix(key, "/"))
		}
		seen[key] = true
	}
	return nil
}
