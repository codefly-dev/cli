package gitops

import (
	"context"
	"crypto/ed25519"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// wikiContract declares a solution's own kind and the roles within it in its
// module contract, the one declaration site of both.
const wikiContract = `schema: codefly/module-contract/v1
principal: shop
namespaces: []
queues: []
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
bindings: []
destinations: []
`

func writeContract(t *testing.T, document string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, modulecontract.FileName), []byte(document), 0o644))
	return dir
}

var (
	wikiPage = &authorityv1.WorkScopeV1{ResourceKind: "wiki.page", Actions: []string{"read", "write", "publish"}}
	// wikiRoles is what the authority's document carries for wikiContract —
	// exactly what the retired roles.codefly.yaml declaring the same roles
	// delivered.
	wikiRoles = []*compositionv1.RoleV1{
		{Name: modulecontract.InstallerRole, Scopes: []*authorityv1.WorkScopeV1{wikiPage}},
		{Name: "editor", Scopes: []*authorityv1.WorkScopeV1{{ResourceKind: "wiki.page", Actions: []string{"read", "write"}}}},
		{Name: "viewer", Scopes: []*authorityv1.WorkScopeV1{{ResourceKind: "wiki.page", Actions: []string{"read"}}}},
	}
)

func requireRoles(t *testing.T, want, got []*compositionv1.RoleV1) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		require.True(t, proto.Equal(want[i], got[i]), "role %d: %v, want %v", i, got[i], want[i])
	}
}

// A solution's roles are read from its module contract, in order, each scope
// as written; its own kinds are its contract's scope ceilings, once. A module
// publishing no contract, or a contract with no roles, declares none, and a
// constructed module has nowhere to ship one.
func TestASolutionsRolesAreReadFromItsModuleContract(t *testing.T) {
	for name, dir := range map[string]string{"no contract": t.TempDir(), "no directory": ""} {
		roles, err := Roles(dir)
		require.NoError(t, err, name)
		require.Nil(t, roles, name)
	}
	dir := writeContract(t, wikiContract)
	roles, err := Roles(dir)
	require.NoError(t, err)
	requireRoles(t, wikiRoles, roles)
	ceilings, err := ScopeCeilings(dir)
	require.NoError(t, err)
	require.Len(t, ceilings, 1, "a kind is declared once, whatever else the contract carries")
	require.True(t, proto.Equal(wikiPage, ceilings[0]))

	bare := writeContract(t, strings.SplitN(wikiContract, "roles:", 2)[0]+"bindings: []\ndestinations: []\n")
	roles, err = Roles(bare)
	require.NoError(t, err)
	require.Nil(t, roles)
}

// Core's rules are the CLI's: a role core's reader refuses is refused by both
// of the delivery's readers, naming the role, where the author reads it.
func TestARolesCoreRefusesIsRefusedByTheDelivery(t *testing.T) {
	for name, c := range map[string]struct{ document, reason string }{
		"no installer":                      {strings.Replace(wikiContract, "name: installer", "name: owner", 1), `none is "installer"`},
		"a role beyond the kind's actions":  {strings.Replace(wikiContract, "actions: [read, write]}", "actions: [read, write, delete]}", 1), `role "editor" grants wiki.page:delete`},
		"a role on a kind nothing asks for": {strings.Replace(wikiContract, "{resource_kind: wiki.page, actions: [read]}", "{resource_kind: documents, actions: [read]}", 1), `role "viewer" grants documents:read`},
		"a kind that starts with a digit":   {strings.ReplaceAll(wikiContract, "wiki.page", "2wiki.page"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeContract(t, c.document)
			_, rolesErr := Roles(dir)
			_, ceilingsErr := ScopeCeilings(dir)
			if c.reason == "" {
				// One kind grammar: core's, which accepts a kind the
				// retired declaration's own grammar refused.
				require.NoError(t, rolesErr)
				require.NoError(t, ceilingsErr)
				return
			}
			require.ErrorIs(t, rolesErr, modulecontract.ErrInvalid)
			require.ErrorContains(t, rolesErr, c.reason)
			require.ErrorContains(t, ceilingsErr, c.reason)
		})
	}
}

// A unit still shipping the retired roles declaration is refused by every
// reader, naming the file and where its contents now belong, so a role is
// never dropped in silence.
func TestTheRetiredRolesDeclarationIsRefusedByName(t *testing.T) {
	dir := writeContract(t, wikiContract)
	retired := filepath.Join(dir, "roles.codefly.yaml")
	require.NoError(t, os.WriteFile(retired, []byte("schema: codefly/solution-roles/v1\n"), 0o644))
	_, rolesErr := Roles(dir)
	_, ceilingsErr := ScopeCeilings(dir)
	for _, err := range []error{rolesErr, ceilingsErr} {
		require.ErrorContains(t, err, retired)
		require.ErrorContains(t, err, modulecontract.FileName)
	}
}

// The roles reach the authority's document (AuthorityManifestV1.roles) exactly
// as declared, and the solution's own kind reaches its scope ceilings once.
func TestContractRolesReachTheAuthorityDocument(t *testing.T) {
	doc := deliveredAuthorityDocument(t, writeContract(t, wikiContract))
	requireRoles(t, wikiRoles, doc.GetRoles())
	require.Len(t, doc.GetScopeCeilings(), 1)
	require.True(t, proto.Equal(wikiPage, doc.GetScopeCeilings()[0]))
}

// ownedContract declares a viewer that reads the wiki's own pages and, of the
// collections it consumes through a binding, only those its installation owns.
const ownedContract = `schema: codefly/module-contract/v1
principal: shop
namespaces: []
queues: []
scope_ceilings:
  - resource_kind: wiki.page
    actions: [read, write]
roles:
  - name: installer
    scopes:
      - {resource_kind: wiki.page, actions: [read, write]}
    installation_owned_scopes:
      - {resource_kind: documents.collection, actions: [read, write]}
  - name: viewer
    scopes:
      - {resource_kind: wiki.page, actions: [read]}
    installation_owned_scopes:
      - {resource_kind: documents.collection, actions: [read]}
bindings:
  - id: collections
    operations: [invoke]
    audience: {from: shop/collections-audience}
    scope_ceiling:
      invoke:
        - {resource_kind: documents.collection, actions: [read, write]}
destinations: []
`

// A role's installation_owned_scopes reach the authority's document as
// RoleV1.installation_owned_scopes (field 3), apart from its scopes and naming
// no ids, so the authority limits each to what the installation owns rather
// than granting the whole kind.
func TestInstallationOwnedScopesReachTheAuthorityDocument(t *testing.T) {
	doc := deliveredAuthorityDocument(t, writeContract(t, ownedContract))
	requireRoles(t, []*compositionv1.RoleV1{
		{
			Name:                    modulecontract.InstallerRole,
			Scopes:                  []*authorityv1.WorkScopeV1{{ResourceKind: "wiki.page", Actions: []string{"read", "write"}}},
			InstallationOwnedScopes: []*authorityv1.WorkScopeV1{{ResourceKind: "documents.collection", Actions: []string{"read", "write"}}},
		},
		{
			Name:                    "viewer",
			Scopes:                  []*authorityv1.WorkScopeV1{{ResourceKind: "wiki.page", Actions: []string{"read"}}},
			InstallationOwnedScopes: []*authorityv1.WorkScopeV1{{ResourceKind: "documents.collection", Actions: []string{"read"}}},
		},
	}, doc.GetRoles())

	// Core's rules hold the installation-owned scopes too: one naming ids has
	// no field for them, and one beyond what the unit asks for is refused,
	// both before anything is delivered.
	for name, c := range map[string]struct{ document, reason string }{
		"ids":           {strings.Replace(ownedContract, "{resource_kind: documents.collection, actions: [read]}", "{resource_kind: documents.collection, actions: [read], resource_ids: [a]}", 1), "resource_ids"},
		"beyond a kind": {strings.Replace(ownedContract, "{resource_kind: documents.collection, actions: [read]}", "{resource_kind: documents, actions: [read]}", 1), `role "viewer" grants installation-owned documents:read`},
	} {
		_, err := Roles(writeContract(t, c.document))
		require.ErrorContains(t, err, c.reason, name)
	}
}

// deliveredAuthorityDocument delivers a solution whose module contract is at
// dir through the composition delivery and returns the authority document the
// host received.
func deliveredAuthorityDocument(t *testing.T, dir string) *compositionv1.AuthorityManifestV1 {
	t.Helper()
	stub := &compositionStub{held: map[string]uint64{}}
	server := grpc.NewServer()
	compositionv1.RegisterCompositionServiceServer(server, stub)
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	_, private, _ := ed25519.GenerateKey(nil)
	signer, err := solutionhost.NewInMemorySigner("release", private)
	require.NoError(t, err)
	d := NewCompositionDelivery(conn, signer,
		func(context.Context, string) ([]*compositionv1.MethodPolicyV1, bool, error) { return nil, false, nil },
		func(context.Context, string) ([]*authorityv1.WorkScopeV1, error) { return ScopeCeilings(dir) },
		func(context.Context, string) ([]*compositionv1.RoleV1, error) { return Roles(dir) })
	manifest := &solutionhost.ModuleManifest{
		Schema: solutionhost.SchemaModuleManifestV1, Binding: "shop", Alias: "shop", Kind: solutionhost.KindSolution, Generation: 1,
		Host: solutionhost.HostTarget{Coordinate: "local", Domain: "local"}, Release: solutionhost.Release{Publisher: "test", Name: "shop", Version: "0.1.0"},
		Workloads: []solutionhost.Workload{{Service: "api", Container: "app"}}, Authority: &solutionhost.Authority{Principal: "shop", ResourceKinds: []string{"wiki.page"}},
	}
	document, err := solutionhost.SignedPayload(manifest)
	require.NoError(t, err)
	_, err = send(t.Context(), d, "shop", &solutionhost.Signed{Document: document, Envelope: []byte(`{}`)}, "delivery-credential")
	require.NoError(t, err)
	var doc compositionv1.AuthorityManifestV1
	require.NoError(t, proto.Unmarshal(stub.seen[0].GetAuthority().GetDocument(), &doc))
	return &doc
}
