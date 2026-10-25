package gitops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/codefly-dev/core/composition"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"google.golang.org/protobuf/proto"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/cli/pkg/methodpolicy"
)

// MethodPolicies reads what each method a module's published protobuf contracts
// serve declares with the authority's policy option, from the module package
// at dir. Every served method is listed, a method with no mark with neither
// field set, so the authority tells "declares nothing" from "not served". A
// method a published descriptor declares but no endpoint of the module serves
// (a protocol another module serves, say) is not listed: the module answers
// for no call to it. A module with no package declares no method policy: ok is
// false.
func MethodPolicies(dir string) (methods []*compositionv1.MethodPolicyV1, ok bool, err error) {
	served, ok, err := ServiceMethodPolicies(dir)
	if err != nil || !ok {
		return nil, ok, err
	}
	return served.All(), true, nil
}

// All is the whole module's table: every service's served methods, each
// once, and the foreign methods, sorted by method.
func (served *ServedMethodPolicies) All() []*compositionv1.MethodPolicyV1 {
	methods := append([]*compositionv1.MethodPolicyV1(nil), served.Foreign...)
	seen := map[string]bool{}
	for _, service := range served.Services {
		for _, m := range served.ByService[service] {
			if !seen[m.GetMethod()] {
				seen[m.GetMethod()] = true
				methods = append(methods, m)
			}
		}
	}
	sort.Slice(methods, func(i, j int) bool { return methods[i].GetMethod() < methods[j].GetMethod() })
	return methods
}

// ServedMethodPolicies is a module's method policies by where they are
// served: each service's published contracts, and the module contract's
// foreign methods, which name no service.
type ServedMethodPolicies struct {
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
// the service whose published contract serves them. A method two services
// serve must carry one policy, and a foreign method no published contract may
// serve, exactly as MethodPolicies refuses.
//
// What a protobuf endpoint serves is what the API contract catalog
// (contracts/api/catalog.codefly.json) records for it: the services and
// procedures its listener registers, which `codefly generate contracts` writes
// from the module's declared surface, or as every service of the endpoint's
// package when it declares none. A descriptor carries more than that, its
// package's other services and its imports', so the rest is Unserved. A
// package with no catalog falls back to that same default.
func ServiceMethodPolicies(dir string) (served ServedMethodPolicies, ok bool, err error) {
	foreign, err := foreignMethods(dir)
	if err != nil {
		return served, false, err
	}
	served.Foreign = foreign
	served.ByService = map[string][]*compositionv1.MethodPolicyV1{}
	if _, statErr := os.Stat(filepath.Join(dir, composition.PackageManifestFileName)); os.IsNotExist(statErr) {
		return served, len(foreign) > 0, nil
	}
	manifest, err := composition.LoadPackageManifest(dir)
	if err != nil {
		return served, false, err
	}
	catalog, err := methodpolicy.LoadCatalog(dir)
	if err != nil {
		return served, false, err
	}
	seen := map[string]*compositionv1.MethodPolicyV1{}
	unserved := map[string]bool{}
	for index := range manifest.Services {
		service := &manifest.Services[index]
		own := map[string]bool{}
		for _, contract := range service.APIContracts {
			methods, skipped, err := methodpolicy.Contract(dir, catalog, service.Name, &contract)
			if err != nil {
				return served, false, fmt.Errorf("module package %s, service %s: %w", manifest.ID, service.Name, err)
			}
			for _, method := range skipped {
				unserved[method] = true
			}
			for _, m := range methods {
				if previous := seen[m.GetMethod()]; previous == nil {
					seen[m.GetMethod()] = m
				} else if !proto.Equal(previous, m) {
					return served, false, fmt.Errorf("module package %s: conflicting policies for %s", manifest.ID, m.GetMethod())
				}
				if !own[m.GetMethod()] {
					own[m.GetMethod()] = true
					served.ByService[service.Name] = append(served.ByService[service.Name], m)
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
		if seen[m.GetMethod()] != nil {
			return served, false, fmt.Errorf("module package %s declares foreign method %s, which its published contracts already serve", manifest.ID, m.GetMethod())
		}
		seen[m.GetMethod()] = m
	}
	// A method one endpoint serves and another's descriptor merely carries is
	// served.
	for method := range unserved {
		if seen[method] == nil {
			served.Unserved = append(served.Unserved, method)
		}
	}
	sort.Strings(served.Unserved)
	return served, true, nil
}

// foreignMethods reads the methods the module at dir serves from a protocol
// it does not own, from its module contract, each with what a call requires.
// A module that publishes no contract declares none.
func foreignMethods(dir string) ([]*compositionv1.MethodPolicyV1, error) {
	contract, err := modulecontract.Load(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*compositionv1.MethodPolicyV1
	for _, method := range contract.ForeignMethods {
		m := &compositionv1.MethodPolicyV1{Method: method.Method}
		for _, required := range method.Requires {
			m.Requires = append(m.Requires, &authorityv1.WorkScopeV1{ResourceKind: required.ResourceKind, Actions: required.Actions})
		}
		out = append(out, m)
	}
	return out, nil
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
