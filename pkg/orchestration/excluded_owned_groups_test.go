package orchestration

import (
	"slices"
	"testing"

	"github.com/codefly-dev/core/resources"
)

// A group the run excludes is not handed to the render-owned renderer either:
// an excluded module-authority is not rendered, and so not refused for a host
// that has no gateway to render it from. The service itself is not mutated.
func TestAnExcludedGroupIsNotRendered(t *testing.T) {
	service := &resources.Service{Name: "page", WorkspaceConfigurationDependencies: []string{"module-authority", "org-directory", "authority"}}
	world := &World{excludedWorkspaceConfigurations: map[string]bool{"module-authority": true}}
	seen := world.withoutExcludedGroups(service)
	if slices.Contains(seen.WorkspaceConfigurationDependencies, "module-authority") || len(seen.WorkspaceConfigurationDependencies) != 2 {
		t.Fatalf("rendered with %v", seen.WorkspaceConfigurationDependencies)
	}
	if len(service.WorkspaceConfigurationDependencies) != 3 {
		t.Fatalf("the declared service was mutated: %v", service.WorkspaceConfigurationDependencies)
	}
	if untouched := (&World{}).withoutExcludedGroups(service); untouched != service {
		t.Fatal("a run excluding nothing copied the service")
	}
}
