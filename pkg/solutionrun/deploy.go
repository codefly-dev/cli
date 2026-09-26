package solutionrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
)

// --- Deployed federation ---
//
// DerivedRunInputs provisions a composed solution's federation for a process it
// is about to start: it mints the secrets, hands the plaintext to the processes
// over Start overrides, and declares the digests to the registrar. None of that
// can reach a deployment. A GitOps render is committed to a repository, so a
// minted secret written into it is a published secret, and a digest declared
// from a render would authorize a value nobody can deliver. Before this, the
// render therefore carried neither half: a deployed solution booted with no
// api.consumes projection and no registration secret, never registered the
// prefixes it consumes, and every request it proxied to a consumed module had
// no route.
//
// A render carries the same carriers under the same names, split by what they
// are. The projection and the prefix a module federates under are public: they
// name routes, not credentials, and are rendered as values. Every secret is
// rendered as a reference only — the environment's secret store holds the value,
// the render names the key — so the plaintext never enters the tree. The digests
// the registrar checks them against already reach it the same way, as secret
// references in the federation workspace configuration group, so a deployment
// is wired by writing the secret and its digest into the store once, never by
// rendering either.

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
// carriers a deployed solution and the modules it consumes need to federate:
//
//   - the solution's entry service gets the api.consumes projection, and a
//     reference to the registration secrets it registers its consumed prefixes
//     with;
//   - every service of a consumed module gets the prefix it federates under, and
//     a reference to its own identity secret — under the canonical carrier and
//     the deprecated alias alike, both resolving to the one stored identity
//     secret, never to the registration secret.
//
// It walks the whole workspace rather than the module being rendered because the
// two ends of one exchange render separately: a consumed module's services are
// rendered in their own module's tree, and only the solution's manifest says
// they are consumed.
//
// The rules are DerivedRunInputs'. A registrar module holds the digests and
// mints work contexts, so it is never handed a secret; a workspace with no
// registrar withholds every secret, since nothing could admit one. A consumed
// module bound under two different prefixes has two identities and one pair of
// carriers to hold them, so the render refuses it instead of picking one.
func DerivedDeployInputs(ctx context.Context, workspace *resources.Workspace) (DeployInputs, error) {
	inputs := DeployInputs{Services: map[string]ServiceInjection{}}
	registrars := federationRegistrars(ctx, workspace)
	holdsDigests := registrarModules(registrars)
	// boundPrefix records the prefix each consumed module's services were bound
	// to, and by which solution, so a second binding can be checked against it.
	type binding struct{ prefix, solution string }
	boundPrefix := map[string]binding{}
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

		federated, hosted := federatedConsumedAPIs(consumed, holdsDigests)
		if len(hosted) > 0 {
			inputs.Notes = append(inputs.Notes, Note{Message: fmt.Sprintf(
				"consumed modules %s declare the %q group and hold the digests: the host routes their APIs itself, so %s federates no prefix for them",
				strings.Join(hosted, ", "), federationConfigurationGroup, mod.Name)})
		}
		bindings := consumedModuleBindings(federated)
		if len(bindings) == 0 {
			continue
		}
		prefixes := make([]string, 0, len(bindings))
		for _, bound := range bindings {
			prefixes = append(prefixes, bound.prefix)
		}
		if len(registrars) == 0 {
			inputs.Notes = append(inputs.Notes, Note{Warning: true, Message: fmt.Sprintf(
				"no service declares the %q workspace configuration: solution %s renders without registration secrets, so its consumed modules (%s) cannot federate",
				federationConfigurationGroup, mod.Name, strings.Join(prefixes, ", "))})
			continue
		}
		inputs.secret(entry, moduleRegistrationSecretsEnvironmentVariable, moduleRegistrationSecretsEnvironmentVariable)
		inputs.Notes = append(inputs.Notes, Note{Message: fmt.Sprintf(
			"rendering %s into %s and a reference to %s: the environment's secret store must hold the `prefix:secret` entries for %s, whose digests the registrar reads from the %q group",
			manifest.APIConsumesEnvironmentVariable, entry, moduleRegistrationSecretsEnvironmentVariable,
			strings.Join(prefixes, ", "), federationConfigurationGroup)})

		// federatedConsumedAPIs already dropped every consumed module that holds the
		// digests, so no binding here is a registrar's.
		for _, bound := range bindings {
			if earlier, seen := boundPrefix[bound.module]; seen {
				if earlier.prefix != bound.prefix {
					return DeployInputs{}, fmt.Errorf(
						"module %s is consumed under the prefix %q by solution %s and %q by solution %s; a module federates under one prefix, which is its identity",
						bound.module, earlier.prefix, earlier.solution, bound.prefix, mod.Name)
				}
				continue
			}
			boundPrefix[bound.module] = binding{prefix: bound.prefix, solution: mod.Name}
			services, err := moduleServiceUniques(ctx, workspace, bound.module)
			if err != nil {
				inputs.Notes = append(inputs.Notes, Note{Warning: true, Message: fmt.Sprintf(
					"no %s rendered for %s (%v): that module cannot obtain a work context, so its module-facing workers will idle",
					moduleIdentitySecretEnvironmentVariable, bound.module, err)})
				continue
			}
			for _, unique := range services {
				inputs.public(unique, moduleIdentityPrefixEnvironmentVariable, bound.prefix)
				inputs.secret(unique, moduleIdentitySecretEnvironmentVariable, moduleIdentitySecretEnvironmentVariable)
				// The deprecated alias carries the identity secret, exactly as a
				// run injects it: one stored value, two names, never the
				// registration secret.
				inputs.secret(unique, moduleRegistrationSecretEnvironmentVariable, moduleIdentitySecretEnvironmentVariable)
			}
			inputs.Notes = append(inputs.Notes, Note{Message: fmt.Sprintf(
				"rendering %s=%s and a reference to %s into the services of %s",
				moduleIdentityPrefixEnvironmentVariable, bound.prefix, moduleIdentitySecretEnvironmentVariable, bound.module)})
		}
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

func (inputs *DeployInputs) secret(unique, key, storeKey string) {
	injection := inputs.Services[unique]
	if injection.Secrets == nil {
		injection.Secrets = map[string]string{}
	}
	injection.Secrets[key] = storeKey
	inputs.Services[unique] = injection
}
