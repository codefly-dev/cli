package solutionshape

import (
	"fmt"
	"sort"
	"strings"

	"github.com/codefly-dev/core/composition"
	modulecontract "github.com/codefly-dev/core/contracts/module"

	"github.com/codefly-dev/cli/pkg/methodpolicy"
)

// This file asks the question the permissions checks did not: not "is this
// grant declared?" but "does anything the role can reach need it?".
//
// Core's Contract.Validate refuses a role granting a scope nothing backs —
// neither the solution's own ceilings nor a scope one of its bindings spells
// out. That catches the missing permission. It cannot catch the opposite, and
// the opposite is what actually happened: obin-ai/core-solutions' wiki
// declared `installer` as `editor` plus a publish action, so whoever installed
// the wiki received full document read and write, and whole-kind collection
// create, as a side effect of installing it. Every check passed, because every
// grant was declared. Nothing asked whether any operation the role reaches
// needs it, and three roles rendered as three near-identical fifty-row tables
// nobody could read against each other.
//
// What is asked here, per role, is one thing: for each action the role grants
// on a resource kind the SOLUTION ITSELF owns, does some operation the
// solution publishes require it? The solution's own kinds are the only ones
// whose checks are the solution's own — the route policies in its published
// contract are where it spells them, and the authority admits a call to a
// route by them. An action no route spells is checked by nothing, so granting
// it reaches nothing.
//
// A kind that comes from a binding is deliberately not judged. The module at
// the other end authorizes the relayed child, its policies are that module's
// and not in this directory, and a role grants a binding's scope precisely so
// the child carries it. Judging those is how a check that "once shipped and
// failed wiki and proposals, the only two solutions known to work" went wrong
// (see checkInstallerRole); this one is scoped to the half the directory can
// actually answer.

const (
	// CodeRoleScopeUnreached names a role granting an action on one of the
	// solution's own kinds that no operation it publishes requires.
	CodeRoleScopeUnreached = "role_grants_unreached_scope"
	// CodeOperationPoliciesUnread names the case where the published
	// operations could not be read, so the question above was not answered.
	// Reported rather than skipped: a run that judged nothing must not read
	// as a run that found nothing wrong.
	CodeOperationPoliciesUnread = "operation_policies_unread"
)

// checkRoleScopesAreReached reports every action a role grants on the
// solution's own kinds that no published operation requires.
//
// It is a WARNING, not a failure. Surplus authority refuses nothing and
// breaks nothing at runtime — it is wider than it needs to be, which is a
// decision to argue and not a broken solution — and this package's bar for a
// failure is a rule whose breach silently broke something. It is also read
// from a GENERATED artifact: `codefly generate contracts` writes the published
// contract, and a tree whose contract is behind its code would make a
// legitimate grant look surplus. Failing a gate on that would be worse than
// the thing it reports.
func (r *Report) checkRoleScopesAreReached(dir string, contract *modulecontract.Contract, packaged *composition.PackageManifest) {
	if len(contract.Roles) == 0 {
		return // checkInstallerRole answers for a solution with no role.
	}
	own := ownKinds(contract)
	if len(own) == 0 {
		r.ok(CodeRoleScopeUnreached, fmt.Sprintf("the solution declares no kinds of its own, so each of its %d role(s) grants only what a binding carries", len(contract.Roles)))
		return
	}

	if !packageDeclaresAContract(packaged) {
		// checkBackendContract already failed, with the remediation. Saying it
		// a second time in another vocabulary helps nobody.
		return
	}

	required, operations, err := publishedRequirements(dir, packaged)
	switch {
	case err != nil:
		r.warn(CodeOperationPoliciesUnread,
			fmt.Sprintf("cannot read what the published operations require: %v", err),
			"the authority derives every route's policy from the same contracts, so whatever refuses this read refuses that derivation too. `codefly generate contracts` rewrites them")
		return
	case operations == 0:
		r.warn(CodeOperationPoliciesUnread,
			"the published contracts carry no operation, so what the roles reach cannot be judged",
			"a solution's roles are spelled in the policies of the routes it publishes; with no operation published there is nothing to compare them against. Run `codefly generate contracts`")
		return
	}
	r.ok(CodeOperationPoliciesUnread, fmt.Sprintf("the published contracts carry %d operation(s) to judge the roles against", operations))

	var surplus []string
	for _, role := range contract.Roles {
		for _, scopes := range [][]modulecontract.CeilingScope{role.Scopes, role.InstallationOwnedScopes} {
			for _, scope := range scopes {
				if !own[scope.ResourceKind] {
					continue // a binding's kind: the module at the other end checks it.
				}
				for _, action := range scope.Actions {
					if required[scope.ResourceKind+":"+action] {
						continue
					}
					surplus = append(surplus, fmt.Sprintf("%s grants %s:%s", role.Name, scope.ResourceKind, action))
				}
			}
		}
	}
	if len(surplus) == 0 {
		r.ok(CodeRoleScopeUnreached, fmt.Sprintf("every action the %d role(s) grant on the solution's own kinds is required by an operation it publishes", len(contract.Roles)))
		return
	}
	sort.Strings(surplus)
	r.warn(CodeRoleScopeUnreached,
		fmt.Sprintf("no operation this solution publishes requires what these role grants carry: %s", strings.Join(surplus, ", ")),
		"each names an action on one of the solution's OWN kinds, so the solution itself is what would check it — and no route policy it publishes spells it, so nothing ever will. Either a route is missing its policy, or the role is wider than the solution: narrow the role, with the reason beside what it keeps")
}

// packageDeclaresAContract reports whether the package publishes any API
// contract at all, which is where the route policies live.
func packageDeclaresAContract(packaged *composition.PackageManifest) bool {
	for index := range packaged.Services {
		if len(packaged.Services[index].APIContracts) > 0 {
			return true
		}
	}
	return false
}

// ownKinds is the resource kinds the solution contributes itself — the ones
// its own route policies are spelled in. A role granting any other kind is
// granting what a binding carries, which this directory cannot judge.
func ownKinds(contract *modulecontract.Contract) map[string]bool {
	kinds := map[string]bool{}
	for _, ceiling := range contract.ScopeCeilings {
		kinds[ceiling.ResourceKind] = true
	}
	return kinds
}

// publishedRequirements is every "<kind>:<action>" some operation of the
// solution's published contracts requires, with the number of operations read,
// so a contract carrying none is told from one whose operations all admit any
// checked caller. It reads with the same reader the authority's own
// method-policy derivation uses (pkg/methodpolicy), so what is judged here is
// exactly what a host would enforce.
func publishedRequirements(dir string, packaged *composition.PackageManifest) (map[string]bool, int, error) {
	catalog, err := methodpolicy.LoadCatalog(dir)
	if err != nil {
		return nil, 0, err
	}
	required := map[string]bool{}
	operations := 0
	for index := range packaged.Services {
		service := &packaged.Services[index]
		for position := range service.APIContracts {
			contract := &service.APIContracts[position]
			methods, _, err := methodpolicy.Contract(dir, catalog, service.Name, contract)
			if err != nil {
				return nil, 0, fmt.Errorf("service %s, contract %s: %w", service.Name, contract.Path, err)
			}
			operations += len(methods)
			for _, method := range methods {
				for _, scope := range method.GetRequires() {
					for _, action := range scope.GetActions() {
						required[scope.GetResourceKind()+":"+action] = true
					}
				}
			}
		}
	}
	return required, operations, nil
}
