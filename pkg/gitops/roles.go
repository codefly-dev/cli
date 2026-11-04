package gitops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	modulecontract "github.com/codefly-dev/core/contracts/module"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// retiredRolesFileName is the declaration a solution shipped beside its
// solution manifest before core's module contract carried roles: its own
// kinds and its roles, under a schema this CLI defined. Nothing reads it now;
// a unit still shipping one is refused naming it, so a role is never dropped
// in silence.
const retiredRolesFileName = "roles.codefly.yaml"

// refuseRetiredRoles refuses the module at dir when it still ships the retired
// roles declaration, saying where its contents now belong.
func refuseRetiredRoles(dir string) error {
	if dir == "" {
		return nil
	}
	path := filepath.Join(dir, retiredRolesFileName)
	_, err := os.Stat(path) //nolint:gosec // G703: dir is a module directory the workspace loaded, the same one its contract is read from
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	return fmt.Errorf("%s is no longer read: a unit's own kinds and its roles are declared in its %s, as scope_ceilings and roles (core's module contract); move them there and delete %s", path, modulecontract.FileName, retiredRolesFileName)
}

// Roles reads the roles the module at dir declares in its module contract,
// in the authority's spelling, in the order written: what the authority's
// document carries for it (AuthorityManifestV1.roles). Core's reader holds
// them to its rules — an installer among them, each scope and each
// installation-owned scope within the unit's own kinds or a permission its
// bindings ask for on a person's behalf, an installation-owned scope naming
// no ids. None when it publishes no contract or its contract declares none.
func Roles(dir string) ([]*compositionv1.RoleV1, error) {
	contract, err := ownContract(dir)
	if err != nil || contract == nil {
		return nil, err
	}
	var out []*compositionv1.RoleV1
	for _, role := range contract.Roles {
		out = append(out, &compositionv1.RoleV1{
			Name:                    role.Name,
			Scopes:                  workScopes(role.Scopes),
			InstallationOwnedScopes: workScopes(role.InstallationOwnedScopes),
		})
	}
	return out, nil
}

// InstallationDeclaration reads a module's installation declaration from its
// own contract: the agent's standing role selector and the modules it reaches
// with the person's Work Context, for delivery to an environment of class
// class (an empty class is an environment that declares none).
//
// The selector is carried VERBATIM. Core validated it already
// (contracts/module rules.go: strict literal selector, no trim or case-fold, a
// named selector held to a declared role), and the paired agreement rule
// compares this document against the Core signed manifest byte-for-byte —
// empty and "none" differ, nil and [] are equivalent zero edges. So nothing
// here trims, defaults, sorts or deduplicates, and an edge's Module stays the
// signed principal the host resolves through its catalogue, never a name, a
// binding id or a release.
//
// An edge's PRESENCE is resolved, through core's one resolution
// (ResolveEdgeDependencies), for exactly the reason the rest is verbatim: the
// Core signed manifest carries the presence this environment's class gives it
// (gitops.moduleAuthority resolves the contract with the same class), and an
// agreement rule compares the two carriers. Carrying the declaration verbatim
// in this half would make every contract whose presence differs by class
// disagree with itself at install.
//
// No contract means no declaration, which is distinct from a contract
// declaring none.
func InstallationDeclaration(dir, class string) (string, []*compositionv1.EdgeDependencyV1, error) {
	contract, err := ownContract(dir)
	if err != nil || contract == nil {
		return "", nil, err
	}
	resolved, err := contract.ResolveEdgeDependencies(class)
	if err != nil {
		return "", nil, fmt.Errorf("module %s: %w", contract.Principal, err)
	}
	edges := make([]*compositionv1.EdgeDependencyV1, 0, len(resolved))
	for _, edge := range resolved {
		edges = append(edges, &compositionv1.EdgeDependencyV1{Module: edge.Module, Presence: edge.Presence})
	}
	if len(edges) == 0 {
		// nil and [] are equivalent zero edges, and a repeated proto field
		// cannot carry the difference; deliver nil so the two spellings
		// serialize identically.
		edges = nil
	}
	return contract.InstallationRole, edges, nil
}

// OwnerAware reads the module's explicit declaration that it checks resource
// owners. No contract or an omitted declaration means false, never an inference
// from the presence of roles or installation-owned scopes.
func OwnerAware(dir string) (bool, error) {
	contract, err := ownContract(dir)
	if err != nil || contract == nil {
		return false, err
	}
	return contract.OwnerAware, nil
}

// workScopes spells contract scopes as the authority's: a kind and actions,
// never ids.
func workScopes(scopes []modulecontract.CeilingScope) []*authorityv1.WorkScopeV1 {
	if len(scopes) == 0 {
		return nil
	}
	out := make([]*authorityv1.WorkScopeV1, 0, len(scopes))
	for _, scope := range scopes {
		out = append(out, &authorityv1.WorkScopeV1{ResourceKind: scope.ResourceKind, Actions: append([]string(nil), scope.Actions...)})
	}
	return out
}

// ownContract reads the module contract at dir for its own vocabulary and
// roles: nil when it publishes none, and a refusal when it still ships the
// retired roles declaration.
func ownContract(dir string) (*modulecontract.Contract, error) {
	if err := refuseRetiredRoles(dir); err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, nil
	}
	contract, err := modulecontract.Load(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return contract, err
}
