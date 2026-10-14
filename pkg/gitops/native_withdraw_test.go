package gitops

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// What withdraws a native delivery is the same generation a publication
// settles for a withdrawal: the presence's tombstone (removed, the next
// generation, no frontend and no group) and the authority's (removed, the
// next generation, nothing describing the module left on it), both valid to
// core.
func TestWithdrawnDocumentsAreTheTombstonesAPublicationSettles(t *testing.T) {
	delivered := deliveredNativeDocuments(t)
	withdrawn, err := WithdrawnDocuments(delivered)
	require.NoError(t, err)
	require.True(t, withdrawn.Presence.Removed)
	require.Equal(t, delivered.Presence.Generation+1, withdrawn.Presence.Generation)
	require.Equal(t, delivered.Presence.Binding, withdrawn.Presence.Binding)
	require.Nil(t, withdrawn.Presence.Frontend)
	require.Nil(t, withdrawn.Presence.Group)
	require.NoError(t, withdrawn.Presence.Validate())
	require.False(t, delivered.Presence.Removed, "the delivered documents are untouched")
	if delivered.Authority != nil {
		require.True(t, withdrawn.Authority.Removed)
		require.Equal(t, delivered.Authority.Generation+1, withdrawn.Authority.Generation)
		require.Empty(t, withdrawn.Authority.Principals)
		require.Empty(t, withdrawn.Authority.ScopeCeilings)
		require.NoError(t, withdrawn.Authority.Validate())
	}
	_, err = WithdrawnDocuments(&NativeDocuments{})
	require.Error(t, err, "nothing delivered is nothing to withdraw")
}

// deliveredNativeDocuments renders the documents a native run delivers for
// the crm fixture, a solution with a page, as a withdrawal would find them.
func deliveredNativeDocuments(t *testing.T) *NativeDocuments {
	t.Helper()
	env := identityEnvironment()
	module, service := nativeSolution(t, "crm")
	documents, err := (&NativeDeliverer{Workspace: groupedWorkspace("Operations", "crm"), Env: env}).Render(t.Context(), &NativeWorkload{
		Module: module, Service: service, Services: []*resources.Service{service},
		Subject: "system:serviceaccount:example:crm-api", SPIFFEID: "spiffe://cluster.example/ns/example/sa/crm-api",
		Artifact: NativeArtifact{Executable: module.Dir() + "/services/api/binary"}, FrontendBundle: builtBundle(),
	})
	require.NoError(t, err)
	require.NotNil(t, documents.Presence.Frontend)
	require.NotNil(t, documents.Presence.Group)
	documents.Presence.Generation = 3
	return documents
}
