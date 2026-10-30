package gitops

import (
	"context"
	"crypto/ed25519"
	"strings"
	"testing"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	"github.com/codefly-dev/authority/libraries/authority-core/go/authority"
	"github.com/codefly-dev/authority/libraries/authority-core/go/decide"
	"github.com/codefly-dev/authority/libraries/authority-core/go/domain"
	"github.com/codefly-dev/authority/libraries/authority-core/go/memstore"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/workcontext"
	"github.com/stretchr/testify/require"
)

// Exercise the real authority issuer with the document received over the CLI's
// composition transport. Only the wire-to-domain projection is test assembly;
// issuance, role ceilings, owner selection, signing and verification are real.
func TestDeliveredOwnerAwareSolutionIssuesForPerson(t *testing.T) {
	for _, declaration := range []string{"", "owner_aware: false\n", "owner_aware: true\n"} {
		t.Run(strings.TrimSpace(declaration), func(t *testing.T) {
			ctx := t.Context()
			doc := deliveredAuthorityDocument(t, writeContract(t, ownedContract+declaration))
			m := domain.Module{
				Name: doc.GetModule(), Principal: doc.GetModule(), Kind: domain.KindSolution,
				Generation: doc.GetGeneration(), OwnerAware: doc.GetOwnerAware(),
				ResourceKinds: issuedScopes(doc.GetScopeCeilings()),
				Roles:         map[string][]*basev0.WorkScopeV1{}, OwnedRoles: map[string][]*basev0.WorkScopeV1{},
				Bindings: map[string]domain.Binding{"collections": {ID: "collections", Audience: "documents", InvokeScopes: []*basev0.WorkScopeV1{{ResourceKind: "documents.collection", Actions: []string{"read", "write"}}}}},
			}
			for _, role := range doc.GetRoles() {
				m.Roles[role.GetName()] = issuedScopes(role.GetScopes())
				m.OwnedRoles[role.GetName()] = issuedScopes(role.GetInstallationOwnedScopes())
			}
			require.NoError(t, decide.ValidateModule(m))
			store := memstore.New()
			store.PutOrganization(domain.Organization{ID: "acme"})
			store.PutPerson(domain.Person{ID: "person", Organization: "acme"})
			inst := domain.Installation{ID: "installation", Organization: "acme", Target: m.Name, PrincipalID: "shop-principal", Status: domain.Active, Revision: 1, Owned: map[string][]string{"documents.collection": {"owned-1"}}}
			require.NoError(t, store.Write(ctx, func(tx authority.Tx) error {
				if err := tx.PutModule(m); err != nil {
					return err
				}
				if err := tx.PutInstallation(inst); err != nil {
					return err
				}
				return tx.PutRoleGrant(domain.RoleGrant{InstallationID: inst.ID, PersonID: "person", Role: "viewer"})
			}))
			public, private, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			svc, err := authority.New(store, authority.Config{Issuer: "test-authority", KeyID: "test-key", Signer: private, Keys: issuanceKeys{public}, Replay: workcontext.NewMemoryReplayStore()})
			require.NoError(t, err)
			issued, err := svc.IssueForPerson(ctx, authority.Person{ID: "person", Organization: "acme", SessionID: "session"}, authority.IssueRequest{Audience: m.Principal})
			if declaration != "owner_aware: true\n" {
				require.ErrorContains(t, err, "shop does not declare owner-aware checking")
				require.Empty(t, issued.Token)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, issued.Token)
			verified, _, err := svc.Check(ctx, issued.Token, m.Principal)
			require.NoError(t, err)
			require.Equal(t, "person", verified.Context().GetOwnerPrincipalId())
			require.Equal(t, inst.ID, verified.Context().GetSeal().GetInstallationId())
			scopes := verified.EffectiveScopes()
			require.Len(t, scopes, 1)
			require.Equal(t, "wiki.page", scopes[0].GetResourceKind())
			require.Equal(t, []string{"read"}, scopes[0].GetActions())
			require.Equal(t, inst.PrincipalID, scopes[0].GetResourceOwnerId())
			// The delivered owned scope stays bounded to this installation's IDs.
			roleScopes, err := decide.RoleScopes(m, inst, []string{"viewer"})
			require.NoError(t, err)
			require.Len(t, roleScopes, 2)
			for _, scope := range roleScopes {
				if scope.GetResourceKind() == "documents.collection" {
					require.Equal(t, []string{"owned-1"}, scope.GetResourceIds())
					require.Equal(t, []string{"read"}, scope.GetActions())
				}
			}
		})
	}
}

type issuanceKeys struct{ public ed25519.PublicKey }

func (k issuanceKeys) PublicKeys(context.Context) (map[string]ed25519.PublicKey, error) {
	return map[string]ed25519.PublicKey{"test-key": k.public}, nil
}

func issuedScopes(in []*authorityv1.WorkScopeV1) []*basev0.WorkScopeV1 {
	out := make([]*basev0.WorkScopeV1, 0, len(in))
	for _, scope := range in {
		out = append(out, &basev0.WorkScopeV1{ResourceKind: scope.GetResourceKind(), Actions: scope.GetActions(), ResourceIds: scope.GetResourceIds(), ResourceOwnerId: scope.GetResourceOwnerId()})
	}
	return out
}

func TestOwnerAwareReadFailureStopsDelivery(t *testing.T) {
	dir := writeContract(t, ownedContract+"owner_aware: yes\n")
	d := NewCompositionDelivery(nil, nativeSigner(t), nil, nil, nil,
		func(context.Context, string) (bool, error) { return OwnerAware(dir) }, nil)
	_, err := d.Prepare(t.Context(), "shop", false)
	require.ErrorContains(t, err, "owner_aware")
	for _, dir := range []string{"", t.TempDir()} {
		declared, err := OwnerAware(dir)
		require.NoError(t, err)
		require.False(t, declared)
	}
}
