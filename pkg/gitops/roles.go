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
// them to its rules — an installer among them, each scope within the unit's
// own kinds or a permission its bindings ask for on a person's behalf. None
// when it publishes no contract or its contract declares none.
func Roles(dir string) ([]*compositionv1.RoleV1, error) {
	contract, err := ownContract(dir)
	if err != nil || contract == nil {
		return nil, err
	}
	var out []*compositionv1.RoleV1
	for _, role := range contract.Roles {
		granted := &compositionv1.RoleV1{Name: role.Name}
		for _, scope := range role.Scopes {
			granted.Scopes = append(granted.Scopes, &authorityv1.WorkScopeV1{ResourceKind: scope.ResourceKind, Actions: append([]string(nil), scope.Actions...)})
		}
		out = append(out, granted)
	}
	return out, nil
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
