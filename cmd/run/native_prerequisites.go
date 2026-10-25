package run

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
)

// NativeAuthorityPrerequisites orders native identity workloads after the host
// that admits their signed manifests. It grants no endpoint or configuration
// access; Core's runtime graph owns scheduling and cycle validation.
func NativeAuthorityPrerequisites(env *environments.Environment, runtimeContext string) orchestration.FlowOption {
	var modules []string
	if env == nil || env.Host == nil || runtimeContext == resources.RuntimeContextContainer {
		return orchestration.WithRuntimePrerequisites(nil, nil)
	}
	for _, ref := range []string{env.Host.Delivery, env.Host.Gateway} {
		if module, _, ok := strings.Cut(ref, "/"); ok {
			modules = append(modules, module)
		}
	}
	return orchestration.WithRuntimePrerequisites(modules, func(ctx context.Context, workspace *resources.Workspace) (map[string][]string, error) {
		if err := env.Host.Validate(); err != nil {
			return nil, err
		}
		hosts, err := nativeHostServices(ctx, workspace, env)
		if err != nil {
			return nil, err
		}
		identities, err := gitops.ResolveNativeIdentities(ctx, workspace, env)
		if err != nil {
			return nil, err
		}
		hostModule := strings.Split(env.Host.Delivery, "/")[0]
		prerequisites := map[string][]string{}
		for unique, identity := range identities {
			if identity.Module != hostModule {
				prerequisites[unique] = hosts
			}
		}
		return prerequisites, nil
	})
}

// The delivery endpoint selects both transport and readiness. Composition
// delivery calls the host directly; only legacy envelope delivery uses a gateway.
func nativeHostServices(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) ([]string, error) {
	parts := strings.Split(env.Host.Delivery, "/")
	if len(parts) != 3 {
		return nil, fmt.Errorf("host.delivery %q is not <module>/<service>/<endpoint>", env.Host.Delivery)
	}
	hosts := []string{parts[0] + "/" + parts[1]}
	if !gitops.HostImplements(ctx, workspace, parts[0], parts[1], parts[2], gitops.CompositionInterface) && env.Host.Gateway != "" {
		hosts = append(hosts, env.Host.Gateway)
	}
	return hosts, nil
}
