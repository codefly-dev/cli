package solutionrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
)

// --- Deployed solutions ---
//
// DerivedRunInputs hands a composed solution its api.consumes projection over a
// Start override, for a process it is about to start. A GitOps render has no
// process to hand it to: a deployed solution reads the projection from the tree
// the render commits, so it is derived here under the same carrier and with the
// same value a run injects, and rendered into the entry service's ConfigMap.
// Before this, a deployed solution booted with no projection, never registered
// the routes it consumes, and every request it proxied to a consumed module had
// no upstream.
//
// The projection names routes, not credentials, so it renders as a value. A
// render never carries a secret value: the tree is committed to a repository,
// so a value written into it is published.

// ServiceInjection is what a render adds to one service's container: Public
// values rendered into the service's ConfigMap, and Secrets rendered as
// secretKeyRefs, keyed by the environment variable and naming the key the
// environment's secret store holds the value under.
type ServiceInjection struct {
	Public  map[string]string
	Secrets map[string]string
}

// DeployInputs is what a render derives for a workspace's composed solutions:
// the injection for each service, keyed by its module-qualified unique, and what
// the derivation wants an operator told.
type DeployInputs struct {
	Services map[string]ServiceInjection
	Notes    []Note
}

// For returns the injection derived for the service with the given unique, or
// the zero injection when none was.
func (inputs DeployInputs) For(unique string) ServiceInjection {
	return inputs.Services[unique]
}

// DerivedDeployInputs resolves, for every solution the workspace composes, the
// api.consumes projection its entry service federates the consumed modules'
// routes from: CODEFLY__API_CONSUMES, as a public value on the entry's unique,
// with the value a run injects.
//
// It walks the whole workspace rather than the module being rendered because
// only a solution's manifest says what it consumes, and each solution renders
// in its own module's tree. A manifest the run would refuse fails the render
// the same way, naming the solution.
func DerivedDeployInputs(ctx context.Context, workspace *resources.Workspace) (DeployInputs, error) {
	inputs := DeployInputs{Services: map[string]ServiceInjection{}}
	for _, ref := range workspace.Modules {
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil || mod.ServiceEntry == "" {
			continue
		}
		solutionManifest, err := moduleManifest(mod)
		if err != nil {
			return DeployInputs{}, fmt.Errorf("solution %s: %w", mod.Name, err)
		}
		if solutionManifest == nil {
			continue
		}
		consumed := solutionManifest.ConsumedAPIs()
		if len(consumed) == 0 {
			continue
		}
		entry := resources.ServiceUnique(mod.Name, mod.ServiceEntry)
		inputs.public(entry, manifest.APIConsumesEnvironmentVariable, solutionManifest.ConsumedAPIsEnvValue())
		inputs.Notes = append(inputs.Notes, Note{Message: fmt.Sprintf("rendering %s into %s: %s",
			manifest.APIConsumesEnvironmentVariable, entry, strings.Join(consumedIDs(consumed), ", "))})
	}
	return inputs, nil
}

func (inputs *DeployInputs) public(unique, key, value string) {
	injection := inputs.Services[unique]
	if injection.Public == nil {
		injection.Public = map[string]string{}
	}
	injection.Public[key] = value
	inputs.Services[unique] = injection
}
