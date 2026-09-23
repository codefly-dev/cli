package orchestration

import (
	"context"
	"fmt"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// SelfEndpointEnvironmentVariables returns a service's self-endpoint carriers —
// core's CODEFLY__SELF_ENDPOINT__<MODULE>__<SERVICE>__<ENDPOINT>__<API>
// (resources.SelfEndpointPrefix) — for its own network mappings, selecting in
// each the instance whose access is networkAccess: the address the rest of the
// deployment reaches the service at, as opposed to its CODEFLY__ENDPOINT__
// carrier, which is the address it listens on. Core owns the name, the value's
// shape and the selection (EnvironmentVariableManager.AddSelfEndpoints); this
// only runs them over mappings the CLI holds, so the carrier reaches a process
// through the CLI's own seams — Start overrides and the rendered ConfigMap —
// even while its agent predates the core release that emits it.
//
// Callers pass the access peers use: container access for a Kubernetes render
// (the in-cluster Service DNS name), and the runtime's own access for a local
// run, exactly as core's builder and runtime do. An endpoint with no instance
// of that access gets no carrier rather than a guessed one.
//
// Core carries this from v0.5.6 (codefly-dev/core#640); until that release the
// CLI pins the pull request's commit.
func SelfEndpointEnvironmentVariables(ctx context.Context, mappings []*basev0.NetworkMapping, networkAccess *basev0.NetworkAccess) map[string]string {
	manager := resources.NewEnvironmentVariableManager()
	if err := manager.AddSelfEndpoints(ctx, mappings, networkAccess); err != nil {
		return nil
	}
	variables := map[string]string{}
	for _, endpoint := range manager.SelfEndpoints() {
		if endpoint.NetworkInstance.GetAddress() == "" {
			continue
		}
		variable := resources.SelfEndpointAsEnvironmentVariable(endpoint)
		variables[variable.Key] = fmt.Sprint(variable.Value)
	}
	if len(variables) == 0 {
		return nil
	}
	return variables
}
