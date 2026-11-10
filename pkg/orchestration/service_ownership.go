package orchestration

import "fmt"

// WithOwnedServices constrains initial manager creation to services exclusively
// owned by the caller. CI holds these locks through Stop and agent-cache eviction.
// Check the live run set too: a changed suite contract or configuration edge
// must fail before it can acquire an agent another task owns.
func (flow *Flow) WithOwnedServices(services []string) {
	flow.ownedServices = make(map[string]bool, len(services))
	for _, service := range services {
		flow.ownedServices[service] = true
	}
}

func (flow *Flow) checkOwnedServices(services []string) error {
	if flow.ownedServices == nil {
		return nil
	}
	for _, service := range services {
		if !flow.ownedServices[service] {
			return fmt.Errorf("flow requires service %s outside its scheduled ownership", service)
		}
	}
	return nil
}
