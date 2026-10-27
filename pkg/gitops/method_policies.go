package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/core/composition"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/proto"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/methodpolicy"
)

// MethodPolicies reads what each method a module's published protobuf contracts
// serve declares with the authority's policy option, from the module package
// at dir: the whole package's table, every service's (ServedMethodPolicies.All).
// Every served method is listed, a method with no mark with neither field set,
// so the authority tells "declares nothing" from "not served". A method a
// published descriptor declares but no endpoint of the module serves (a
// protocol another module serves, say) is not listed: the module answers for
// no call to it. A module with no package declares no method policy: ok is
// false. What a host receives is HostMethodPolicies, which may set services
// aside.
func MethodPolicies(dir string) (methods []*compositionv1.MethodPolicyV1, ok bool, err error) {
	served, ok, err := ServiceMethodPolicies(dir)
	if err != nil || !ok {
		return nil, ok, err
	}
	methods, err = served.All()
	return methods, err == nil, err
}

// HostMethodPolicies is the table a module's manifest carries to env's host,
// and so the one the permission preflight holds and a caller's binding
// resolves against: the package's table less the services that stand in for
// that host, or are it (hostStandIns). Those serve the host's own interfaces,
// so a call to them is the host's to answer and never one the host decides
// for the module, and a composition whose host is another service never runs
// them as the module. A service that runs as the module (module-identity) is
// never set aside, nor is any service of a module that implements no
// interface the host does: every such module's table is MethodPolicies.
func HostMethodPolicies(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, module *resources.Module) ([]*compositionv1.MethodPolicyV1, bool, error) {
	served, ok, err := ServiceMethodPolicies(module.Dir())
	if err != nil || !ok {
		return nil, ok, err
	}
	standIns, err := hostStandIns(ctx, workspace, env, module)
	if err != nil {
		return nil, false, err
	}
	methods, err := served.Table(func(service string) bool { return !standIns[service] })
	if err != nil {
		return nil, false, err
	}
	return methods, true, nil
}

// hostStandIns names the services of module that stand in for env's host: a
// service whose endpoint the module interface exports as implementing a
// Codefly interface the host service (the one host.delivery names) also
// implements, at any version, and which does not run as the module. The
// local authority is one: it implements the host interfaces so a workspace
// with no saas host has one, and a composition that binds those interfaces to
// another host never runs it. In the host's own module the host service is
// named by the same rule. A host the workspace does not compose cannot be
// read, so nothing is set aside toward it: every method stays held, as
// methodPoliciesRequired holds such a host to receive every policy. A host
// module that does not load is an error with its reason.
func hostStandIns(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, module *resources.Module) (map[string]bool, error) {
	if env == nil || env.Host == nil || module.Interface == nil {
		return nil, nil
	}
	hostModule, hostService, _ := env.Host.DeliveryEndpoint()
	host, err := workspace.LoadModuleFromName(ctx, hostModule)
	switch {
	case err != nil && isModuleNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("cannot tell which services of module %s stand in for host %s/%s: its module does not load: %w", module.Name, hostModule, hostService, err)
	}
	hosted := map[string]bool{}
	if host.Interface != nil {
		for _, endpoint := range host.Interface.Endpoints {
			if endpoint.Service != hostService {
				continue
			}
			for _, implemented := range endpoint.Implements {
				hosted[interfaceName(implemented)] = true
			}
		}
	}
	standIns := map[string]bool{}
	for _, endpoint := range module.Interface.Endpoints {
		if standIns[endpoint.Service] || !slices.ContainsFunc(endpoint.Implements, func(implemented string) bool { return hosted[interfaceName(implemented)] }) {
			continue
		}
		service, err := module.LoadServiceFromName(ctx, endpoint.Service)
		if err != nil {
			return nil, fmt.Errorf("module %s service %s implements an interface of host %s/%s and does not load: %w", module.Name, endpoint.Service, hostModule, hostService, err)
		}
		if !service.ModuleIdentity {
			standIns[endpoint.Service] = true
		}
	}
	return standIns, nil
}

// interfaceName is a Codefly interface reference without its version:
// "codefly.dev/authority" for "codefly.dev/authority@0.3.3".
func interfaceName(reference string) string {
	name, _, _ := strings.Cut(reference, "@")
	return name
}

// All is the whole package's table: Table with every service.
func (served *ServedMethodPolicies) All() ([]*compositionv1.MethodPolicyV1, error) {
	return served.Table(nil)
}

// Table is the table of the services keep admits (every service when keep is
// nil), each method once, and the foreign methods, sorted by method. A table
// is one authority's view of one audience, keyed by method alone, so two of
// its services serving one method — an HTTP route, which names no service —
// must declare one policy for it. Two services that are never in one table may
// declare it differently.
func (served *ServedMethodPolicies) Table(keep func(service string) bool) ([]*compositionv1.MethodPolicyV1, error) {
	methods := append([]*compositionv1.MethodPolicyV1(nil), served.Foreign...)
	seen := map[string]*compositionv1.MethodPolicyV1{}
	for _, m := range served.Foreign {
		seen[m.GetMethod()] = m
	}
	for _, service := range served.Services {
		if keep != nil && !keep(service) {
			continue
		}
		for _, m := range served.ByService[service] {
			previous := seen[m.GetMethod()]
			switch {
			case previous == nil:
				seen[m.GetMethod()] = m
				methods = append(methods, m)
			case !proto.Equal(previous, m):
				return nil, fmt.Errorf("module package %s: conflicting policies for %s", served.Package, m.GetMethod())
			}
		}
	}
	sort.Slice(methods, func(i, j int) bool { return methods[i].GetMethod() < methods[j].GetMethod() })
	return methods, nil
}

// ServedMethodPolicies is a module's method policies by where they are
// served: each service's published contracts, and the module contract's
// foreign methods. Explicitly selected foreign methods join ByService;
// unselected entries remain in Foreign for existing consumers.
type ServedMethodPolicies struct {
	// Package is the package's ID, which a refusal names.
	Package string
	// Services are the package's services that publish a contract, in
	// manifest order.
	Services  []string
	ByService map[string][]*compositionv1.MethodPolicyV1
	Foreign   []*compositionv1.MethodPolicyV1
	// Unserved are the methods a published descriptor declares that no
	// endpoint of the module serves, sorted: left out of every table, so a
	// caller can say so.
	Unserved []string
}

// ServiceMethodPolicies reads the same policies as MethodPolicies, kept by
// the service whose published contract serves them. A method one service
// serves on two endpoints must carry one policy there, and a foreign method
// no published contract may serve; a method two services serve meets in a
// table, which refuses it there (Table).
//
// A service's protobuf contracts are read before its OpenAPI ones, and a
// route an OpenAPI operation of the service declares nothing for, which one
// of the service's protobuf methods binds (google.api.http), is that method's
// route with that method's policy: the document is the REST gateway's
// rendering of the proto, which has no place for the policy option, and the
// proto is the one place the module declares both (methodpolicy). An
// operation that declares a policy of its own is a declaration, and one that
// differs from the proto's is refused.
//
// What a protobuf endpoint serves is what the API contract catalog
// (contracts/api/catalog.codefly.json) records for it: the services and
// procedures its listener registers, which `codefly generate contracts` writes
// from the module's declared surface, or as every service of the endpoint's
// package when it declares none. A descriptor carries more than that, its
// package's other services and its imports', so the rest is Unserved. A
// package with no catalog falls back to that same default.
func ServiceMethodPolicies(dir string) (served ServedMethodPolicies, ok bool, err error) {
	foreign, selected, err := foreignMethods(dir)
	if err != nil {
		return served, false, err
	}
	served.ByService = map[string][]*compositionv1.MethodPolicyV1{}
	if _, statErr := os.Stat(filepath.Join(dir, composition.PackageManifestFileName)); os.IsNotExist(statErr) {
		if len(selected) > 0 {
			return served, false, fmt.Errorf("foreign method service selection requires a package manifest")
		}
		served.Foreign = foreign
		return served, len(foreign) > 0, nil
	}
	manifest, err := composition.LoadPackageManifest(dir)
	if err != nil {
		return served, false, err
	}
	served.Package = manifest.ID
	catalog, err := methodpolicy.LoadCatalog(dir)
	if err != nil {
		return served, false, err
	}
	servedAnywhere := map[string]bool{}
	unserved := map[string]bool{}
	for index := range manifest.Services {
		service := &manifest.Services[index]
		contracts := make([]*composition.ProvidedAPIContract, 0, len(service.APIContracts))
		for position := range service.APIContracts {
			contracts = append(contracts, &service.APIContracts[position])
		}
		sort.SliceStable(contracts, func(i, j int) bool {
			return contracts[i].Kind == composition.APIContractKindProtobuf && contracts[j].Kind != composition.APIContractKindProtobuf
		})
		own := map[string]*compositionv1.MethodPolicyV1{}
		protoRoutes := map[string]bool{}
		for _, contract := range contracts {
			methods, skipped, err := methodpolicy.Contract(dir, catalog, service.Name, contract)
			if err != nil {
				return served, false, fmt.Errorf("module package %s, service %s: %w", manifest.ID, service.Name, err)
			}
			for _, method := range skipped {
				unserved[method] = true
			}
			for _, m := range methods {
				servedAnywhere[m.GetMethod()] = true
				previous := own[m.GetMethod()]
				switch {
				case previous == nil:
					own[m.GetMethod()] = m
					if contract.Kind == composition.APIContractKindProtobuf {
						protoRoutes[m.GetMethod()] = true
					}
					served.ByService[service.Name] = append(served.ByService[service.Name], m)
				case proto.Equal(previous, m):
				case contract.Kind == composition.APIContractKindOpenAPI && protoRoutes[m.GetMethod()] && declaresNothing(m):
					// The gateway's copy of a route the service's proto declares.
				default:
					return served, false, fmt.Errorf("module package %s: conflicting policies for %s", manifest.ID, m.GetMethod())
				}
			}
		}
		if methods := served.ByService[service.Name]; len(methods) > 0 {
			sort.Slice(methods, func(i, j int) bool { return methods[i].GetMethod() < methods[j].GetMethod() })
			served.Services = append(served.Services, service.Name)
		}
	}
	// A foreign method is one no published contract serves: a module that
	// declares one it also publishes would have two answers for one method.
	for _, m := range foreign {
		if servedAnywhere[m.GetMethod()] {
			return served, false, fmt.Errorf("module package %s declares foreign method %s, which its published contracts already serve", manifest.ID, m.GetMethod())
		}
		servedAnywhere[m.GetMethod()] = true
		if service := selected[m.GetMethod()]; service != "" {
			if !publishesProtobuf(manifest, service) {
				return served, false, fmt.Errorf("foreign method %s selects service %q, which publishes no protobuf contract", m.GetMethod(), service)
			}
			served.ByService[service] = append(served.ByService[service], m)
			if !slices.Contains(served.Services, service) {
				served.Services = append(served.Services, service)
			}
		} else {
			served.Foreign = append(served.Foreign, m)
		}
	}
	for _, methods := range served.ByService {
		sort.Slice(methods, func(i, j int) bool { return methods[i].GetMethod() < methods[j].GetMethod() })
	}
	// A method one endpoint serves and another's descriptor merely carries is
	// served.
	for method := range unserved {
		if !servedAnywhere[method] {
			served.Unserved = append(served.Unserved, method)
		}
	}
	sort.Strings(served.Unserved)
	return served, true, nil
}

// declaresNothing reports a method listed with neither field set.
func declaresNothing(m *compositionv1.MethodPolicyV1) bool {
	return !m.GetAnyCheckedCaller() && len(m.GetRequires()) == 0
}

// publishesProtobuf reports whether the manifest's service of that name
// publishes a protobuf contract, which is what a foreign method's service
// selector must name: the selector chooses where a method is served, so a
// service serving no protobuf cannot be the answer.
func publishesProtobuf(manifest *composition.PackageManifest, service string) bool {
	for i := range manifest.Services {
		if manifest.Services[i].Name != service {
			continue
		}
		for _, contract := range manifest.Services[i].APIContracts {
			if contract.Kind == composition.APIContractKindProtobuf {
				return true
			}
		}
	}
	return false
}

// foreignMethods reads the methods the module at dir serves from a protocol
// it does not own, from its module contract, each with what a call requires.
// A module that publishes no contract declares none.
func foreignMethods(dir string) ([]*compositionv1.MethodPolicyV1, map[string]string, error) {
	contract, err := modulecontract.Load(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var out []*compositionv1.MethodPolicyV1
	selected := map[string]string{}
	for _, method := range contract.ForeignMethods {
		m := &compositionv1.MethodPolicyV1{Method: method.Method}
		if method.Service != "" {
			selected[method.Method] = method.Service
		}
		for _, required := range method.Requires {
			m.Requires = append(m.Requires, &authorityv1.WorkScopeV1{ResourceKind: required.ResourceKind, Actions: required.Actions})
		}
		out = append(out, m)
	}
	return out, selected, nil
}

// ScopeCeilings reads the module's own resource kinds with the actions declared
// on each, from its module contract's scope_ceilings: the one declaration of
// them, a solution's as much as a module's. A module that publishes no
// contract declares none.
func ScopeCeilings(dir string) ([]*authorityv1.WorkScopeV1, error) {
	contract, err := ownContract(dir)
	if err != nil || contract == nil {
		return nil, err
	}
	out := make([]*authorityv1.WorkScopeV1, 0, len(contract.ScopeCeilings))
	for _, ceiling := range contract.ScopeCeilings {
		out = append(out, &authorityv1.WorkScopeV1{ResourceKind: ceiling.ResourceKind, Actions: append([]string(nil), ceiling.Actions...)})
	}
	return out, nil
}
