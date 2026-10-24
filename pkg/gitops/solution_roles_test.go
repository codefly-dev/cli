package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

const wikiRoles = `schema: codefly/solution-roles/v1
scope_ceilings:
  - resource_kind: wiki.page
    actions: [read, write, publish]
roles:
  - name: installer
    scopes:
      - resource_kind: wiki.page
        actions: [read, write, publish]
  - name: editor
    scopes:
      - {resource_kind: wiki.page, actions: [read, write]}
  - name: viewer
    scopes:
      - {resource_kind: wiki.page, actions: [read]}
`

func writeRoles(t *testing.T, document string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, SolutionRolesFileName), []byte(document), 0o644))
	return dir
}

// A solution's declaration is read as written: its kinds with their actions,
// and its roles in order, each role's scopes within those kinds. A solution
// shipping none declares none, and a constructed module has nowhere to ship one.
func TestASolutionsRolesAreReadFromItsDeclaration(t *testing.T) {
	for name, dir := range map[string]string{"no declaration": t.TempDir(), "no directory": ""} {
		declared, err := SolutionRoles(dir)
		require.NoError(t, err, name)
		require.Nil(t, declared, name)
	}
	declared, err := SolutionRoles(writeRoles(t, wikiRoles))
	require.NoError(t, err)
	page := &authorityv1.WorkScopeV1{ResourceKind: "wiki.page", Actions: []string{"read", "write", "publish"}}
	require.Len(t, declared.ScopeCeilings, 1)
	require.True(t, proto.Equal(page, declared.ScopeCeilings[0]), declared.ScopeCeilings[0])
	require.Len(t, declared.Roles, 3)
	require.True(t, proto.Equal(&compositionv1.RoleV1{Name: InstallerRole, Scopes: []*authorityv1.WorkScopeV1{page}}, declared.Roles[0]))
	require.Equal(t, "editor", declared.Roles[1].GetName())
	require.Equal(t, []string{"read"}, declared.Roles[2].GetScopes()[0].GetActions())
	// The same kinds are the solution's scope ceilings, beside any a module
	// contract declares.
	ceilings, err := ScopeCeilings(writeRoles(t, wikiRoles))
	require.NoError(t, err)
	require.Len(t, ceilings, 1)
	require.True(t, proto.Equal(page, ceilings[0]))
}

// What the declaration gets wrong is refused where its author reads the reason,
// naming the file, the role and the kind.
func TestARolesDeclarationIsHeldToItsRules(t *testing.T) {
	for name, c := range map[string]struct{ document, reason string }{
		"no installer":                     {strings.Replace(wikiRoles, "name: installer", "name: owner", 1), `none is "installer"`},
		"no roles":                         {strings.SplitN(wikiRoles, "roles:", 2)[0] + "roles: []\n", `declares 0 role(s)`},
		"no kinds":                         {"schema: codefly/solution-roles/v1\nscope_ceilings: []\nroles: []\n", "declares the kinds it serves"},
		"a role beyond the kind's actions": {strings.Replace(wikiRoles, "actions: [read, write]}", "actions: [read, write, delete]}", 1), `role "editor" grants wiki.page:delete, beyond the actions`},
		"a role on an undeclared kind":     {strings.Replace(wikiRoles, "{resource_kind: wiki.page, actions: [read]}", "{resource_kind: documents, actions: [read]}", 1), `role "viewer" grants documents, a kind the solution does not declare`},
		"a role named twice":               {strings.Replace(wikiRoles, "name: viewer", "name: editor", 1), `role "editor" is declared twice`},
		"a role granting nothing":          {strings.Replace(wikiRoles, "    scopes:\n      - {resource_kind: wiki.page, actions: [read]}\n", "    scopes: []\n", 1), `role "viewer" grants nothing`},
		"a role not a name":                {strings.Replace(wikiRoles, "name: viewer", "name: Viewer", 1), `role "Viewer" is not a lowercase name`},
		"a kind named twice":               {strings.Replace(wikiRoles, "scope_ceilings:\n", "scope_ceilings:\n  - {resource_kind: wiki.page, actions: [read]}\n", 1), "scope_ceilings names wiki.page twice"},
		"a kind with no action":            {strings.Replace(wikiRoles, "actions: [read, write, publish]\nroles", "actions: []\nroles", 1), "wiki.page declares no action"},
		"another schema":                   {strings.Replace(wikiRoles, "solution-roles/v1", "solution-roles/v2", 1), `schema "codefly/solution-roles/v2" is not codefly/solution-roles/v1`},
		"an unknown key":                   {strings.Replace(wikiRoles, "roles:\n", "kinds: []\nroles:\n", 1), "field kinds not found"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeRoles(t, c.document)
			_, err := SolutionRoles(dir)
			require.ErrorContains(t, err, c.reason)
			require.ErrorContains(t, err, filepath.Join(dir, SolutionRolesFileName))
			// And the delivery's ceilings read the same refusal.
			_, err = ScopeCeilings(dir)
			require.ErrorContains(t, err, c.reason)
		})
	}
}
