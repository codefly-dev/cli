package solutionshape

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/codefly-dev/authority/libraries/authority-core/go/decide"
	"github.com/codefly-dev/authority/libraries/authority-core/go/domain"
	"github.com/codefly-dev/authority/libraries/authority-core/go/scope"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
)

// This file answers, before anything boots, the question a developer
// otherwise reads off the authority's log during a run: for each role, what
// does a holder reach? And its inverse: what does no role reach?
//
// The local authority already asks it at boot, with decide.AdmitMethod, and
// prints "no single declared role covers this method" one line among
// hundreds. The same function decides it here, against the same scopes (a
// role's scopes and its installation-owned scopes, as the delivered document
// carries them) and the same method table (pkg/methodpolicy, the reader the
// delivered table is written with), so this view and the authority cannot
// disagree about the target itself.
//
// It is the DIRECTORY'S view, and says so. Through a binding it shows what a
// role can carry (the role's scopes cut to what the binding spells, as
// issuance cuts a child); what the module at the other end then admits, and
// onward, is the composition's, which the authority's own reach walk answers
// over the delivered modules. Nothing here re-implements that walk.

const (
	// CodeOperationReachedByNoRole names a published operation that requires
	// scopes no declared role carries: no person can ever call it.
	CodeOperationReachedByNoRole = "operation_reached_by_no_role"
	// CodeBindingWiderThanRoles names a person-acting binding asking for
	// what no role carries: the hop can never mint it for anyone.
	CodeBindingWiderThanRoles = "binding_wider_than_roles"
	// CodeInstallationRoleUndeclared names a contract with roles that does
	// not say what standing authority its installation holds.
	CodeInstallationRoleUndeclared = "installation_role_undeclared"
)

// RoleReach is what a holder of one role reaches, as the directory declares it.
type RoleReach struct {
	Role string `json:"role"`
	// Operations are the unit's own published operations that require a
	// scope and that a holder is admitted to, decided by decide.AdmitMethod.
	// An operation admitting any checked caller is every role's, and is
	// counted once on the report instead. Nil when they could not be read.
	Operations []string `json:"operations"`
	// Bindings are what the role can carry through each binding the unit
	// redeems on a person's behalf.
	Bindings []BindingReach `json:"bindings,omitempty"`
	// AsPerson are the modules the unit reaches on the holder's own session
	// (edge dependencies): the holder's own grants there decide.
	AsPerson []string `json:"as_person,omitempty"`
}

// BindingReach is what one role carries through one binding operation.
type BindingReach struct {
	Binding   string `json:"binding"`
	Operation string `json:"operation"`
	// Carries is "kind:action" the role's holder can carry through it.
	Carries []string `json:"carries,omitempty"`
	// ResolvedByComposition says the binding's ceiling is what its calls
	// require (or a kind the composition fills), so Carries is the role's
	// grants outside the unit's own kinds: an upper bound.
	ResolvedByComposition bool `json:"resolved_by_composition,omitempty"`
	// WholeBinding says the role names the binding under its bindings, and
	// so grants everything the binding carries for the operation.
	WholeBinding bool `json:"whole_binding,omitempty"`
}

// personOperations are the binding operations a hop redeems for a holder; a
// headless binding acts for nobody, so it is no holder's reach.
var personOperations = []string{"invoke", "lookup"}

// checkInstallationRole reports a contract with roles that leaves the
// installation's standing authority undeclared. The host refuses to install
// such a unit unless the install request names a role ("declares no
// installation authority; its owner must declare it before installation"),
// long after every document read fine. A module page holds no standing
// authority, so it should say none.
func (r *Report) checkInstallationRole(contract *modulecontract.Contract, modulePage bool) {
	switch selector := contract.InstallationRole; {
	case selector == "":
		r.warn(CodeInstallationRoleUndeclared, "the contract declares roles and no installation_role",
			"a host refuses to install it unless the request names one: \"declares no installation authority; its owner must declare it before installation\". Declare `installation_role: none` — the installation holds no standing grant — unless it must act with one of its own roles")
	case selector == domain.NoInstallationRole:
		r.ok(CodeInstallationRoleUndeclared, "the installation holds no standing authority (installation_role: none)")
	case modulePage:
		r.warn(CodeInstallationRoleUndeclared, fmt.Sprintf("the module page's installation holds role %q as standing authority", selector),
			"a module page is reached by people and holds no standing authority of its own; declare `installation_role: none` unless something must act as the installation")
	default:
		r.ok(CodeInstallationRoleUndeclared, fmt.Sprintf("the installation holds role %q as its standing authority", selector))
	}
}

// judgeRoles computes every role's reach and reports what no role reaches.
// methods is nil when the published operations could not be read; the
// operations half is then left unjudged (checkRoleScopesAreReached has
// already said why) and the bindings half is still answered.
func (r *Report) judgeRoles(contract *modulecontract.Contract, methods map[string]domain.MethodPolicy) {
	if len(contract.Roles) == 0 {
		return
	}
	own := ownKinds(contract)
	target := domain.Module{Name: contract.Principal, Principal: contract.Principal, Methods: methods}
	covered := map[string]bool{}
	carried := map[string]bool{} // "<binding>/<operation>/<kind>:<action>"
	var edges []string
	for _, edge := range contract.EdgeDependencies {
		edges = append(edges, edge.Module)
	}
	for index := range contract.Roles {
		role := &contract.Roles[index]
		held := roleScopes(role)
		reach := RoleReach{Role: role.Name, AsPerson: edges}
		if methods != nil {
			reach.Operations = []string{}
			for _, method := range slices.Sorted(maps.Keys(methods)) {
				if methods[method].AnyCheckedCaller {
					continue
				}
				if decide.AdmitMethod(target, method, held) == nil {
					reach.Operations = append(reach.Operations, method)
					covered[method] = true
				}
			}
		}
		for position := range contract.Bindings {
			binding := &contract.Bindings[position]
			for _, operation := range personOperations {
				if !slices.Contains(binding.Operations, operation) {
					continue
				}
				through, spelled := spelledCeiling(binding, operation)
				whole := grantsBinding(role, binding.ID, operation)
				var carries []*basev0.WorkScopeV1
				switch {
				case whole && spelled:
					carries = through
				case whole:
					carried[binding.ID+"/"+operation+"/*"] = true
				case spelled:
					carries = scope.Intersect(held, through)
				default:
					carries = foreign(held, own)
				}
				names := actionNames(carries)
				for _, name := range names {
					carried[binding.ID+"/"+operation+"/"+name] = true
				}
				if len(names) > 0 || !spelled || whole {
					reach.Bindings = append(reach.Bindings, BindingReach{Binding: binding.ID, Operation: operation, Carries: names, ResolvedByComposition: !spelled, WholeBinding: whole})
				}
			}
		}
		r.Roles = append(r.Roles, reach)
	}
	if methods != nil {
		for _, policy := range methods {
			if policy.AnyCheckedCaller {
				r.OpenOperations++
			}
		}
		r.checkOperationsReached(methods, covered)
	}
	r.checkBindingsCarried(contract, carried)
}

// checkOperationsReached reports every operation that requires scopes no
// role carries. An operation admitting any checked caller needs no role.
func (r *Report) checkOperationsReached(methods map[string]domain.MethodPolicy, covered map[string]bool) {
	var unreached []string
	for _, method := range slices.Sorted(maps.Keys(methods)) {
		if !methods[method].AnyCheckedCaller && !covered[method] {
			unreached = append(unreached, method)
		}
	}
	if len(unreached) == 0 {
		r.ok(CodeOperationReachedByNoRole, fmt.Sprintf("every one of the %d published operation(s) is reached by a role or admits any checked caller", len(methods)))
		return
	}
	r.warn(CodeOperationReachedByNoRole,
		fmt.Sprintf("no role carries what these operations require, so no person can call them: %s", strings.Join(unreached, ", ")),
		"the authority refuses every person's call to them (at boot it logs \"no single declared role covers this method\"). Grant a role what the operation requires, or, when only a machine calls it, say so in its policy")
}

// checkBindingsCarried reports, per binding a hop redeems for a person, what
// it asks for that no role carries. Issuance cuts a child to the holder's
// grants and the binding together, so an action no role grants is never
// carried: the binding is wider than any role needs, and the consent an
// administrator reviews is wider than anything a person can use.
func (r *Report) checkBindingsCarried(contract *modulecontract.Contract, carried map[string]bool) {
	var wider []string
	for index := range contract.Bindings {
		binding := &contract.Bindings[index]
		for _, operation := range personOperations {
			if !slices.Contains(binding.Operations, operation) {
				continue
			}
			through, spelled := spelledCeiling(binding, operation)
			if !spelled {
				// Its ceiling is what its calls require, read from the module
				// at the other end. The directory can still say when no role
				// holds anything outside the unit's own kinds to carry.
				if !anyCarried(carried, binding.ID+"/"+operation+"/") {
					wider = append(wider, fmt.Sprintf("%s (%s) carries nothing for any role", binding.ID, operation))
				}
				continue
			}
			var missing []string
			for _, name := range actionNames(through) {
				if !carried[binding.ID+"/"+operation+"/"+name] {
					missing = append(missing, name)
				}
			}
			if len(missing) > 0 {
				wider = append(wider, fmt.Sprintf("%s (%s) asks %s, which no role grants", binding.ID, operation, strings.Join(missing, ", ")))
			}
		}
	}
	if len(wider) == 0 {
		r.ok(CodeBindingWiderThanRoles, "every binding redeemed for a person carries only what some role grants")
		return
	}
	r.warn(CodeBindingWiderThanRoles,
		fmt.Sprintf("bindings ask for what no role can carry: %s", strings.Join(wider, "; ")),
		"a hop for a person carries the person's grants cut to the binding, so what no role grants is never carried — a call needing it is refused, and the consent reviewed is wider than anyone can use. Narrow the binding to what a role grants, grant it to the role that needs it, or declare the operation headless when the unit acts for nobody")
}

// grantsBinding reports whether role names binding's operation under its
// bindings, which grants everything the binding carries for it.
func grantsBinding(role *modulecontract.Role, binding, operation string) bool {
	return slices.ContainsFunc(role.Bindings, func(granted modulecontract.RoleBinding) bool {
		return granted.Binding == binding && granted.Operation == operation
	})
}

// roleScopes is what a holder of role carries at the unit: its scopes and its
// installation-owned scopes, as the delivered document carries them and the
// authority's boot coverage reads them.
func roleScopes(role *modulecontract.Role) []*basev0.WorkScopeV1 {
	var out []*basev0.WorkScopeV1
	for _, scopes := range [][]modulecontract.CeilingScope{role.Scopes, role.InstallationOwnedScopes} {
		for _, s := range scopes {
			out = append(out, &basev0.WorkScopeV1{ResourceKind: s.ResourceKind, Actions: slices.Clone(s.Actions)})
		}
	}
	return scope.Normalize(out)
}

// spelledCeiling is the binding's ceiling for operation when the contract
// spells its kinds out; false when the composition resolves it (its calls,
// or bare actions on a kind slot).
func spelledCeiling(binding *modulecontract.Binding, operation string) ([]*basev0.WorkScopeV1, bool) {
	ceiling, ok := binding.ScopeCeiling[operation]
	if !ok || len(ceiling.Scopes) == 0 {
		return nil, false
	}
	var out []*basev0.WorkScopeV1
	for _, s := range ceiling.Scopes {
		out = append(out, &basev0.WorkScopeV1{ResourceKind: s.ResourceKind, Actions: slices.Clone(s.Actions)})
	}
	return scope.Normalize(out), true
}

// foreign is held without the unit's own kinds: the most a binding whose
// ceiling the composition resolves could carry.
func foreign(held []*basev0.WorkScopeV1, own map[string]bool) []*basev0.WorkScopeV1 {
	var out []*basev0.WorkScopeV1
	for _, s := range held {
		if !own[s.GetResourceKind()] {
			out = append(out, s)
		}
	}
	return out
}

// actionNames spells scopes as sorted "kind:action".
func actionNames(scopes []*basev0.WorkScopeV1) []string {
	var out []string
	for _, s := range scope.Normalize(scopes) {
		for _, action := range s.GetActions() {
			out = append(out, s.GetResourceKind()+":"+action)
		}
	}
	slices.Sort(out)
	return out
}

func anyCarried(carried map[string]bool, prefix string) bool {
	for key := range carried {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}
