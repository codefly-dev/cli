package gitops

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"gopkg.in/yaml.v3"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// SolutionRolesFileName is the declaration a solution ships beside its solution
// manifest: its own resource kinds with their actions (its scope ceilings, in
// the module contract's spelling) and the roles a person may hold on an
// installation of it, each within those kinds, one of them installer. Core's
// module contract carries scope ceilings and no roles, and a composed solution
// asks for authority from its solution manifest rather than a contract, so
// neither carries them; this file does, until core's full manifest does. The
// render names its kinds in the core manifest (moduleAuthority), and the
// delivery carries kinds and roles in the authority's document (ScopeCeilings,
// CompositionDelivery). A module that ships one is refused by the authority: a
// module's access is the organisation's roles.
const SolutionRolesFileName = "roles.codefly.yaml"

// SolutionRolesSchema is the one schema the declaration is read under.
const SolutionRolesSchema = "codefly/solution-roles/v1"

// InstallerRole is the role every solution declares: whoever installs it holds
// it on that installation.
const InstallerRole = "installer"

// RolesDeclaration is a solution's declaration, read and held to its rules.
type RolesDeclaration struct {
	// ScopeCeilings are the solution's own kinds with the actions on each.
	ScopeCeilings []*authorityv1.WorkScopeV1
	// Roles are the roles it declares, as written, installer among them.
	Roles []*compositionv1.RoleV1
}

type rolesDocument struct {
	Schema        string         `yaml:"schema"`
	ScopeCeilings []ceilingEntry `yaml:"scope_ceilings"`
	Roles         []roleEntry    `yaml:"roles"`
}

type ceilingEntry struct {
	ResourceKind string   `yaml:"resource_kind"`
	Actions      []string `yaml:"actions"`
}

type roleEntry struct {
	Name   string         `yaml:"name"`
	Scopes []ceilingEntry `yaml:"scopes"`
}

var (
	kindName = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	roleName = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
)

// SolutionRoles reads the declaration of the solution at dir, strictly: an
// unknown key, a kind named twice, a role named twice or not lowercase, a role
// that grants nothing, a role beyond the declared kinds and their actions, and
// a declaration with no installer are each refused by name, here where the
// author reads the reason. Nil, nil when the solution ships none.
func SolutionRoles(dir string) (*RolesDeclaration, error) {
	if dir == "" {
		return nil, nil
	}
	path := filepath.Join(dir, SolutionRolesFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var document rolesDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if document.Schema != SolutionRolesSchema {
		return nil, fmt.Errorf("%s: schema %q is not %s", path, document.Schema, SolutionRolesSchema)
	}
	if len(document.ScopeCeilings) == 0 {
		return nil, fmt.Errorf("%s: a solution declares the kinds it serves under scope_ceilings", path)
	}
	declared := &RolesDeclaration{}
	actions := map[string][]string{}
	for _, ceiling := range document.ScopeCeilings {
		if _, twice := actions[ceiling.ResourceKind]; twice {
			return nil, fmt.Errorf("%s: scope_ceilings names %s twice", path, ceiling.ResourceKind)
		}
		if err := checkScope(ceiling); err != nil {
			return nil, fmt.Errorf("%s: scope_ceilings: %w", path, err)
		}
		actions[ceiling.ResourceKind] = ceiling.Actions
		declared.ScopeCeilings = append(declared.ScopeCeilings, &authorityv1.WorkScopeV1{ResourceKind: ceiling.ResourceKind, Actions: slices.Clone(ceiling.Actions)})
	}
	names := map[string]bool{}
	for _, role := range document.Roles {
		switch {
		case !roleName.MatchString(role.Name):
			return nil, fmt.Errorf("%s: role %q is not a lowercase name", path, role.Name)
		case names[role.Name]:
			return nil, fmt.Errorf("%s: role %q is declared twice", path, role.Name)
		case len(role.Scopes) == 0:
			return nil, fmt.Errorf("%s: role %q grants nothing", path, role.Name)
		}
		names[role.Name] = true
		out := &compositionv1.RoleV1{Name: role.Name}
		for _, scope := range role.Scopes {
			if err := checkScope(scope); err != nil {
				return nil, fmt.Errorf("%s: role %q: %w", path, role.Name, err)
			}
			ceiling, served := actions[scope.ResourceKind]
			if !served {
				return nil, fmt.Errorf("%s: role %q grants %s, a kind the solution does not declare under scope_ceilings", path, role.Name, scope.ResourceKind)
			}
			for _, action := range scope.Actions {
				if !slices.Contains(ceiling, action) {
					return nil, fmt.Errorf("%s: role %q grants %s:%s, beyond the actions the solution declares on %s", path, role.Name, scope.ResourceKind, action, scope.ResourceKind)
				}
			}
			out.Scopes = append(out.Scopes, &authorityv1.WorkScopeV1{ResourceKind: scope.ResourceKind, Actions: slices.Clone(scope.Actions)})
		}
		declared.Roles = append(declared.Roles, out)
	}
	if !names[InstallerRole] {
		return nil, fmt.Errorf("%s: a solution declares %d role(s) and none is %q; whoever installs the solution holds it", path, len(document.Roles), InstallerRole)
	}
	return declared, nil
}

// checkScope holds one kind-and-actions entry to its spelling.
func checkScope(scope ceilingEntry) error {
	if !kindName.MatchString(scope.ResourceKind) {
		return fmt.Errorf("resource kind %q is not a lowercase name", scope.ResourceKind)
	}
	if len(scope.Actions) == 0 {
		return fmt.Errorf("%s declares no action", scope.ResourceKind)
	}
	for _, action := range scope.Actions {
		if !kindName.MatchString(action) {
			return fmt.Errorf("%s: action %q is not a lowercase name", scope.ResourceKind, action)
		}
	}
	return nil
}
