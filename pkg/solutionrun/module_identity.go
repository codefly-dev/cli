package solutionrun

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// --- Declared module identities ---
//
// A module's identity used to reach it only as a side effect of federation: a
// solution consuming the module under a facade prefix minted the prefix's
// secrets, and the module's services received the identity half. A module that
// federates no facade still authenticates to its host as its own principal when
// it mints its module Work Context — a worker running a delegated exchange does —
// and received nothing, so it exited with its projected identity missing.
//
// The service that needs the identity declares it: `module-identity: true` in its
// service.codefly.yaml. The declaration is read here rather than the host's own
// list of the principals it admits, because that list is one host's
// configuration, with its own group, key and schema; reading it would teach the
// CLI a product's configuration format. The declaration names no host and no
// product, and it is static, so a render can later derive from it exactly what a
// run does. Which host admits the module, and with what authority, stays the
// host's decision: a digest for a module the host does not admit authorizes
// nothing there.
//
// The identity is the one the module would federate under: the facade prefix a
// solution in the workspace consumes it as, or its module name when none does —
// the same key a host admits it under either way. Registration secrets are
// untouched: they exist only for facade prefixes, so a module without a facade
// gets an identity and no registration secret.

// declaredModulePrincipal is one module whose services declared module-identity:
// the identity it presents, and the services that present it.
type declaredModulePrincipal struct {
	module   string
	prefix   string
	services []string
}

// derivedDeclaredModuleIdentities mints an identity secret for every declared
// module principal the registrar does not already cover, hands the plaintext to
// the declaring services, and declares its digest to the registrar under
// MODULE_IDENTITY_SECRETS. bound are the facade bindings the solution half of
// this run already provisioned; a module among them has its identity already.
func derivedDeclaredModuleIdentities(ctx context.Context, workspace *resources.Workspace, registrars []string, bound []consumedModuleBinding) RunInputs {
	principals, notes := declaredModulePrincipals(ctx, workspace, registrarModules(registrars), bound)
	if len(principals) == 0 {
		return RunInputs{Notes: notes}
	}
	modules := make([]string, 0, len(principals))
	for _, principal := range principals {
		modules = append(modules, principal.module)
	}
	if len(registrars) == 0 {
		// Nothing can admit the identity, so minting one only has the module spend
		// its heartbeat on an exchange that cannot succeed. Say so and boot, as the
		// solution half does.
		notes = append(notes, Note{Warning: true, Message: fmt.Sprintf(
			"no service declares the %q workspace configuration: modules %s declare module-identity but nothing can admit it, so their module-facing workers will idle",
			federationConfigurationGroup, strings.Join(modules, ", "))})
		return RunInputs{Notes: notes}
	}

	overrides := map[string]map[string]string{}
	digests := make([]string, 0, len(principals))
	for _, principal := range principals {
		identity := mintModuleSecret()
		for _, unique := range principal.services {
			overrides[unique] = map[string]string{
				moduleIdentityPrefixEnvironmentVariable:     principal.prefix,
				moduleIdentitySecretEnvironmentVariable:     identity,
				moduleRegistrationSecretEnvironmentVariable: identity,
			}
		}
		digests = append(digests, principal.prefix+":"+moduleSecretDigest(identity))
	}
	notes = append(notes, Note{Message: fmt.Sprintf(
		"provisioned %s and %s into the services of %s that declare module-identity, identity digests into %s",
		moduleIdentityPrefixEnvironmentVariable, moduleIdentitySecretEnvironmentVariable,
		strings.Join(modules, ", "), strings.Join(registrars, ", "))})
	return RunInputs{
		Overrides:               overrides,
		Notes:                   notes,
		WorkspaceConfigurations: federationDeclaration(map[string]string{moduleIdentitySecretsKey: strings.Join(digests, ",")}),
	}
}

// declaredModulePrincipals walks the workspace for modules with at least one
// service declaring module-identity, in workspace order. A registrar's module is
// skipped — it holds the digests and mints work contexts rather than presenting a
// secret for one — as is a module the solution half already bound. Every module
// left out for a reason an operator can act on is reported, never dropped
// silently: a module that receives nothing looks exactly like one whose exchange
// is broken.
//
// A module or service that does not load is skipped, as federationRegistrars
// skips it: a module the run cannot load is not one it can start either.
func declaredModulePrincipals(ctx context.Context, workspace *resources.Workspace, holdsDigests []string, bound []consumedModuleBinding) ([]declaredModulePrincipal, []Note) {
	facades, notes := workspaceFacadeBindings(ctx, workspace, holdsDigests)
	claimed := map[string]string{}
	for _, binding := range append(slices.Clone(bound), facades...) {
		if _, taken := claimed[binding.prefix]; !taken {
			claimed[binding.prefix] = binding.module
		}
	}

	var principals []declaredModulePrincipal
	for _, ref := range workspace.Modules {
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil {
			continue
		}
		if slices.ContainsFunc(bound, func(binding consumedModuleBinding) bool { return binding.module == mod.Name }) {
			continue
		}
		services := declaringServices(ctx, workspace, mod)
		if len(services) == 0 {
			continue
		}
		if slices.Contains(holdsDigests, mod.Name) {
			notes = append(notes, Note{Message: fmt.Sprintf(
				"module %s declares module-identity and the %q group: it holds the digests and mints work contexts, so it is given no identity secret",
				mod.Name, federationConfigurationGroup)})
			continue
		}
		prefix := mod.Name
		if index := slices.IndexFunc(facades, func(binding consumedModuleBinding) bool { return binding.module == mod.Name }); index >= 0 {
			prefix = facades[index].prefix
		}
		if !isRegistrationIdentity(prefix) {
			notes = append(notes, Note{Warning: true, Message: fmt.Sprintf(
				"no %s provisioned for module %s: its identity %q is not one the registrar accepts (lowercase letters, digits and dashes, starting and ending alphanumeric, at most %d characters)",
				moduleIdentitySecretEnvironmentVariable, mod.Name, prefix, registrationIdentityMaxLength)})
			continue
		}
		if owner, taken := claimed[prefix]; taken && owner != mod.Name {
			notes = append(notes, Note{Warning: true, Message: fmt.Sprintf(
				"no %s provisioned for module %s: its identity %q is the facade prefix module %s federates under, and one identity is one principal",
				moduleIdentitySecretEnvironmentVariable, mod.Name, prefix, owner)})
			continue
		}
		claimed[prefix] = mod.Name
		principals = append(principals, declaredModulePrincipal{module: mod.Name, prefix: prefix, services: services})
	}
	return principals, notes
}

// declaringServices returns the module-qualified uniques of the module's services
// that declare module-identity. Only those receive the secret: a sibling service
// that never mints the module's work context has no use for its credential.
func declaringServices(ctx context.Context, workspace *resources.Workspace, mod *resources.Module) []string {
	var services []string
	for _, svcRef := range mod.ServiceReferences {
		svc, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: svcRef.Name, Module: mod.Name})
		if err != nil || !svc.ModuleIdentity {
			continue
		}
		services = append(services, resources.ServiceUnique(mod.Name, svc.Name))
	}
	return services
}

// workspaceFacadeBindings collects the facade prefix every solution in the
// workspace consumes each module under, in workspace order, so a declared module
// presents the identity it federates under even when this run's root is not the
// solution that consumes it. A manifest the run cannot read is reported and
// skipped: another solution's broken manifest must not stop this run, but a
// module whose prefix it held would otherwise fall back to its name unexplained.
func workspaceFacadeBindings(ctx context.Context, workspace *resources.Workspace, holdsDigests []string) ([]consumedModuleBinding, []Note) {
	var bindings []consumedModuleBinding
	var notes []Note
	for _, ref := range workspace.Modules {
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil || mod.ServiceEntry == "" {
			continue
		}
		solutionManifest, err := moduleManifest(mod)
		if err != nil {
			notes = append(notes, Note{Warning: true, Message: fmt.Sprintf(
				"cannot read the solution manifest of %s (%v): a module it consumes under a facade prefix is identified by its module name instead", mod.Name, err)})
			continue
		}
		if solutionManifest == nil {
			continue
		}
		federated, _ := federatedConsumedAPIs(solutionManifest.ConsumedAPIs(), holdsDigests)
		for _, binding := range consumedModuleBindings(federated) {
			if !slices.ContainsFunc(bindings, func(existing consumedModuleBinding) bool { return existing.module == binding.module }) {
				bindings = append(bindings, binding)
			}
		}
	}
	return bindings, notes
}
