package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/configurations"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	solutionmanifest "github.com/codefly-dev/core/solution/manifest"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/wool"
)

// --- What a module asks for ---
//
// A module's manifest carries its authority REQUEST: what its published
// contract (module.contract.codefly.yaml) asks for, or — for a composed
// solution, which declares what it consumes in its solution manifest — what
// its scoped api.consumes entries amount to (core's ConsumptionContract). The
// render resolves the contract's slots against the composition and writes the
// result as the manifest's authority block. It is a request: the host grants
// the administrator's approval for the module's principal, reports what the
// request asks beyond it, and never widens the grant from it. The principal is
// always the module's own name.

// workspaceValues is the provider the render resolves contract slots from:
// the workspace configurations the environment provides, enumerated per group
// exactly as supplied — every record, in both spellings of a key, a key
// supplied twice, a key classified public here and secret there — and nothing
// decided. Normalisation, ambiguity and secret precedence are core's resolution
// rules, once (modulecontract.Values), so two renderers cannot read one
// composition into two authority requests; core's resolution kit fails a
// provider that selects.
type workspaceValues struct {
	provided *configurations.WorkspaceConfigurations
}

// Records implements modulecontract.Values: a group's records as the
// composition supplies them, across every information that contributes to
// the group.
func (values workspaceValues) Records(group string) ([]modulecontract.Record, error) {
	if values.provided == nil {
		return nil, nil
	}
	var records []modulecontract.Record
	for _, info := range values.provided.Infos {
		if info.GetName() != group {
			continue
		}
		for _, value := range info.GetConfigurationValues() {
			records = append(records, modulecontract.Record{Key: value.GetKey(), Value: value.GetValue(), Secret: value.GetSecret()})
		}
	}
	return records, nil
}

// moduleAuthority is the authority block of a module's manifest: its
// principal, always, and what its request asks for, resolved against the
// composition's public configuration.
func moduleAuthority(
	ctx context.Context,
	workspace *resources.Workspace,
	module *resources.Module,
	services []*resources.Service,
	env *environments.Environment,
	request authorityRequest,
) (*solutionhost.Authority, error) {
	authority := &solutionhost.Authority{Principal: module.Name}
	if err := refuseRetiredRoles(module.Dir()); err != nil {
		return nil, fmt.Errorf("module %s: %w", module.Name, err)
	}
	if request.undeclared != "" {
		return authority, nil
	}
	// The principal is the module's own name, and the contract is held to it
	// before anything resolves: a module claiming another module's principal
	// would claim its grant, and a binding addressed to the module itself
	// ({self: true}) resolves to the contract's principal — this name.
	if request.contract.Principal != module.Name {
		return nil, fmt.Errorf("module %s publishes a contract whose principal is %q; the principal every credential of a module is issued to is the module's own name, so the contract declares principal: %s", module.Name, request.contract.Principal, module.Name)
	}
	provided, err := configurations.ReadWorkspaceConfigurations(ctx, workspace, env.Runtime(), orchestration.ModuleArtifactResolverFor(workspace))
	if err != nil {
		return nil, fmt.Errorf("read the workspace configurations the contract of %s resolves against: %w", module.Name, err)
	}
	callees := newComposedCallees(func(name string) ([]*compositionv1.MethodPolicyV1, bool, error) {
		callee, loadErr := workspace.LoadModuleFromName(ctx, name)
		if loadErr != nil {
			return nil, false, fmt.Errorf("it is not a module this workspace composes: %w", loadErr)
		}
		return HostMethodPolicies(ctx, workspace, env, callee)
	})
	resolved, err := request.contract.Resolve(workspaceValues{provided: provided}, modulecontract.WithCallees(callees))
	if err != nil {
		return nil, fmt.Errorf("module %s: %w", module.Name, err)
	}
	if err := refuseUnaddressableConsumption(workspace, module, request, resolved); err != nil {
		return nil, err
	}
	if err := refuseUndeclaredDestinations(module.Name, resolved, services); err != nil {
		return nil, err
	}
	// An optional binding the composition fills none of the slots of is left
	// out of the delivered manifest by core's resolution, so no credential can
	// be issued through it; the render says so rather than drop it silently.
	for _, omitted := range resolved.Omitted {
		wool.Get(ctx).In("gitops.moduleAuthority").Info(
			fmt.Sprintf("module %s: optional binding %s is omitted from the delivered manifest: %s", module.Name, omitted.ID, omitted.Reason),
			wool.Field("module", module.Name), wool.Field("binding", omitted.ID))
	}
	// A binding addressed to a list of audiences is delivered once per listed
	// audience, each its own binding (core's AudienceBindingID) with the
	// ceiling that audience's own declarations give it; the render says which,
	// since the delivered ids are not the ones the contract spells.
	for i := range request.contract.Bindings {
		declared := &request.contract.Bindings[i]
		if declared.Audiences == nil {
			continue
		}
		var delivered []string
		for _, binding := range resolved.Bindings {
			if strings.HasPrefix(binding.ID, modulecontract.AudienceBindingID(declared.ID, "")) {
				delivered = append(delivered, binding.ID)
			}
		}
		if len(delivered) > 0 {
			wool.Get(ctx).In("gitops.moduleAuthority").Info(
				fmt.Sprintf("module %s: binding %s is delivered once per listed audience, as %s", module.Name, declared.ID, strings.Join(delivered, ", ")),
				wool.Field("module", module.Name), wool.Field("binding", declared.ID))
		}
	}
	for _, binding := range resolved.Bindings {
		operations := make(map[string][]string, len(binding.Operations))
		for _, operation := range binding.Operations {
			operations[operation] = append([]string(nil), binding.Scopes[operation]...)
		}
		authority.Bindings = append(authority.Bindings, solutionhost.AuthorityBinding{ID: binding.ID, Audience: binding.Audience, Operations: operations})
	}
	// A module that installs prepared bindings also asks for each one it
	// installs (runnable_grants.go).
	if installsPreparedBindings(services) {
		grants, err := preparedGrants(provided)
		if err != nil {
			return nil, fmt.Errorf("module %s: %w", module.Name, err)
		}
		if err := appendPreparedBindings(authority, module.Name, grants); err != nil {
			return nil, err
		}
	}
	authority.Queues = append([]string(nil), resolved.Queues...)
	authority.Namespaces = append([]string(nil), resolved.Namespaces...)
	for _, ceiling := range resolved.ScopeCeilings {
		if !slices.Contains(authority.ResourceKinds, ceiling.ResourceKind) {
			authority.ResourceKinds = append(authority.ResourceKinds, ceiling.ResourceKind)
		}
	}
	return authority, nil
}

// authorityRequest is what a module asks for authority with, from exactly one
// source: its published module contract, or — for a composed solution, which
// declares what it consumes in its solution manifest — the scopes its
// api.consumes entries ask for. undeclared carries the reason when it asks for
// none; source says which file asked, for a refusal.
type authorityRequest struct {
	contract *modulecontract.Contract
	// consumed is the scoped api.consumes entries the contract was built
	// from, when the request is a solution's consumption; nil for a module's
	// own contract.
	consumed   []solutionmanifest.APIDeclaration
	source     string
	undeclared string
}

// authorityRequestOf reads a module's request for authority, as a module
// contract either way: the one it publishes, or the one a composed solution's
// scoped api.consumes amounts to (core's ConsumptionContract). The caller
// resolves both through the same Resolve, so a solution's audience comes from
// the composition exactly as a module's does.
//
// The solution manifest is read through core's LoadConsumption: the schema
// version and api.consumes, strictly, and nothing else. An invalid or unknown
// field in a consumption fails the render; a section with nothing to do with
// consumption — an agent block, a section this core does not model — never
// does, because a render of a solution that asks for no authority must not fail
// on what an authority read does not read. The run path's lenient read is
// unchanged.
//
// Both sources at once is refused, by name. A solution that is also consumed
// may publish a contract for its destinations; merging its bindings with a
// derived consumption is a decision about two declarations of one request that
// nothing has made yet, so for now it declares its consumption in one place.
func authorityRequestOf(module *resources.Module) (authorityRequest, error) {
	contract, err := modulecontract.Load(module.Dir())
	switch {
	case errors.Is(err, os.ErrNotExist):
		contract = nil
	case err != nil:
		return authorityRequest{}, err
	}
	var consumption *modulecontract.Contract
	var consumed []solutionmanifest.APIDeclaration
	data, err := os.ReadFile(filepath.Join(module.Dir(), solutionmanifest.FileName))
	switch {
	case module.Dir() == "":
		// A module constructed rather than loaded has nowhere to ship a
		// manifest; reading the working directory instead would be a guess.
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return authorityRequest{}, fmt.Errorf("read the solution manifest of module %s: %w", module.Name, err)
	default:
		declared, loadErr := solutionmanifest.LoadConsumption(data)
		if loadErr != nil {
			return authorityRequest{}, fmt.Errorf("module %s: the api.consumes an authority request is derived from: %w", module.Name, loadErr)
		}
		// A composed solution is a module, and its principal is the name it is
		// composed under, exactly as a module contract's principal must be.
		if consumption, err = declared.ConsumptionContract(module.Name); err != nil {
			return authorityRequest{}, fmt.Errorf("module %s: %w", module.Name, err)
		}
		for index := range declared.API.Consumes {
			if len(declared.API.Consumes[index].Scopes) > 0 {
				consumed = append(consumed, declared.API.Consumes[index])
			}
		}
	}
	switch {
	case contract != nil && consumption != nil && len(consumption.Bindings) > 0:
		return authorityRequest{}, fmt.Errorf("module %s publishes %s and its %s asks for scopes on api.consumes; a module asks for authority from one declaration, so declare the consumption in one of them", module.Name, modulecontract.FileName, solutionmanifest.FileName)
	case contract != nil:
		return authorityRequest{contract: contract, source: "publishes " + modulecontract.FileName}, nil
	case consumption != nil:
		source := "declares admission in its " + solutionmanifest.FileName
		if len(consumption.Bindings) > 0 {
			source = "asks for scopes on api.consumes in its " + solutionmanifest.FileName
		}
		return authorityRequest{contract: consumption, consumed: consumed, source: source}, nil
	}
	return authorityRequest{undeclared: fmt.Sprintf("module %s publishes no %s and asks for no scopes on api.consumes, so it asks for no authority", module.Name, modulecontract.FileName)}, nil
}

// refuseUnaddressableConsumption refuses a solution's request the host could
// never install, here, where its author reads the reason: a scoped entry for a
// module the composition does not compose, and one addressed to the solution
// itself — by name, or by a resolved audience that is its own principal — which
// the host omits from its registry with only a logged reason.
func refuseUnaddressableConsumption(workspace *resources.Workspace, module *resources.Module, request authorityRequest, resolved *modulecontract.Resolved) error {
	if request.consumed == nil {
		return nil
	}
	composed := map[string]bool{}
	for _, reference := range workspace.Modules {
		if reference != nil {
			composed[reference.Name] = true
		}
	}
	for index := range request.consumed {
		declaration := &request.consumed[index]
		if declaration.Module == module.Name {
			return fmt.Errorf("module %s asks for scopes at itself on api.consumes entry %s; a credential is never addressed to its own principal, and the host would never install the binding", module.Name, declaration.ID)
		}
		if !composed[declaration.Module] {
			return fmt.Errorf("module %s asks for scopes at module %s on api.consumes entry %s, which this workspace does not compose; a binding for a module nothing composes can never be installed", module.Name, declaration.Module, declaration.ID)
		}
	}
	for _, binding := range resolved.Bindings {
		if binding.Audience == resolved.Principal {
			return fmt.Errorf("module %s asks for scopes on api.consumes entry %s at audience %q, which is its own principal; a credential is never addressed to its own principal, and the host would never install the binding", module.Name, binding.ID, binding.Audience)
		}
	}
	return nil
}

// errUnrenderedDestination is the refusal of a contract the reader accepted
// whose destination the module does not declare: the renderer's verdict past
// the reader, named so a conformance run can tell it from any other.
var errUnrenderedDestination = errors.New("the contract declares a destination the module does not declare")

// refuseUndeclaredDestinations holds each destination the contract declares to
// the endpoints the module's services declare: a destination naming an
// endpoint nothing declares is one nothing can ever route, refused by name.
func refuseUndeclaredDestinations(module string, contract *modulecontract.Resolved, services []*resources.Service) error {
	declared := map[string]bool{}
	for _, service := range services {
		for _, endpoint := range service.Endpoints {
			if endpoint != nil {
				declared[service.Name+"/"+endpoint.Name] = true
			}
		}
	}
	var missing []string
	for _, destination := range contract.Destinations {
		if !declared[destination.Service+"/"+destination.Endpoint] {
			missing = append(missing, fmt.Sprintf("%s (%s/%s)", destination.ID, destination.Service, destination.Endpoint))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: module %s declares the destinations %s, and none of its services declares such an endpoint; declare the endpoint on the service or withdraw the destination", errUnrenderedDestination, module, strings.Join(missing, ", "))
}

// composedCallees is modulecontract.Callees over the composed modules: what a
// call to one requires is that module's own method policy for it, read from
// its published contracts, so a binding that names its calls carries exactly
// what the callee declares and the caller names none of its kinds. A callee's
// policies are read once per resolution.
type composedCallees struct {
	read     func(module string) ([]*compositionv1.MethodPolicyV1, bool, error)
	policies map[string]map[string]*compositionv1.MethodPolicyV1
}

func newComposedCallees(read func(module string) ([]*compositionv1.MethodPolicyV1, bool, error)) *composedCallees {
	return &composedCallees{read: read, policies: map[string]map[string]*compositionv1.MethodPolicyV1{}}
}

// Requires implements modulecontract.Callees. A call the audience does not
// serve, or one it serves declaring nothing (which the hop refuses), is an
// error naming it; a call open to any checked caller requires no scope.
func (c *composedCallees) Requires(audience, call string) ([]string, error) {
	methods, held := c.policies[audience]
	if !held {
		served, declared, err := c.read(audience)
		if err != nil {
			return nil, err
		}
		if !declared {
			return nil, fmt.Errorf("%s publishes no contracts, so what its calls require cannot be read", audience)
		}
		methods = make(map[string]*compositionv1.MethodPolicyV1, len(served))
		for _, method := range served {
			methods[method.GetMethod()] = method
		}
		c.policies[audience] = methods
	}
	method, served := methods[call]
	switch {
	case !served:
		return nil, fmt.Errorf("%s does not serve it", audience)
	case method.GetAnyCheckedCaller():
		return nil, nil
	case len(method.GetRequires()) == 0:
		return nil, fmt.Errorf("%s declares nothing for it, and the hop refuses a call to a method that declares nothing", audience)
	}
	var scopes []string
	for _, required := range method.GetRequires() {
		for _, action := range required.GetActions() {
			scopes = append(scopes, required.GetResourceKind()+":"+action)
		}
	}
	return scopes, nil
}
