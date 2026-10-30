package orchestration

import (
	"slices"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
)

// LocalRuntimeServices lists the selected services whose local agents will
// receive Start. Unlike the status plan, it excludes remote and no-op origins.
// Call after InitManagers has resolved the test dependency policy.
func (flow *Flow) LocalRuntimeServices() []string {
	if flow == nil {
		return nil
	}
	if selected := flow.runtimeParticipants.Load(); selected != nil {
		return slices.Clone(*selected)
	}
	return nil
}

// Publish only at accepted lifecycle boundaries; delivery goroutines read an
// immutable snapshot without racing extension's mutable manager/service lists.
func (flow *Flow) publishRuntimeServices() {
	selected := flow.selectedRuntimeServices()
	flow.runtimeParticipants.Store(&selected)
	declared := slices.Clone(selected)
	if flow.externalIdentityOrigin() {
		unique := resources.WithUnique(flow.originService).Unique()
		if !slices.Contains(declared, unique) {
			declared = append(declared, unique)
		}
	}
	flow.authorityParticipants.Store(&declared)
}

func (flow *Flow) selectedRuntimeServices() []string {
	if flow == nil || flow.world == nil || flow.validationSkipped ||
		(flow.world.Mode != RunMode && flow.world.Mode != TestMode) {
		return nil
	}
	if flow.world.Mode == TestMode {
		switch flow.testDependencyMode {
		case agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_DEPENDENCIES, agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK:
		default:
			return nil
		}
	}
	excluded := map[string]bool{}
	for _, remote := range flow.remoteServices {
		excluded[remote.Unique()] = true
	}
	for _, unique := range flow.excludedDependencyServices {
		excluded[unique] = true
	}
	if flow.originService != nil && (flow.excludeRoot || (flow.world.Mode == TestMode && !flow.OriginStartsForTest())) {
		excluded[resources.WithUnique(flow.originService).Unique()] = true
	}
	var selected []string
	for _, service := range flow.services {
		if service == nil {
			continue
		}
		unique := resources.WithUnique(service).Unique()
		if !excluded[unique] {
			selected = append(selected, unique)
			excluded[unique] = true
		}
	}
	return selected
}

// LocalAuthorityServices also declares an excluded root driven by a dependency
// client. LocalRuntimeServices remains the actual agent Start set.
func (flow *Flow) LocalAuthorityServices() []string {
	if flow != nil {
		if selected := flow.authorityParticipants.Load(); selected != nil {
			return slices.Clone(*selected)
		}
	}
	return nil
}

func (flow *Flow) externalIdentityOrigin() bool {
	if flow == nil || flow.world == nil || flow.world.Mode != RunMode || flow.validationSkipped ||
		!flow.excludeRoot || flow.originService == nil || !flow.originService.ModuleIdentity {
		return false
	}
	unique := resources.WithUnique(flow.originService).Unique()
	for _, remote := range flow.remoteServices {
		if remote.Unique() == unique {
			return false
		}
	}
	return true
}
