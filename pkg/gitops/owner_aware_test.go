package gitops

import (
	"context"
	"strings"
	"testing"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/stretchr/testify/require"
)

// What the CLI delivers over its composition transport carries owner-aware
// checking exactly as the contract declares it — absent and false alike read
// false — beside the roles and the installation-owned scopes as declared.
// What the authority issues for each is the authority's, proven in
// authority-core (an owner-unaware solution is issued nothing for a person).
func TestTheDeliveredDocumentCarriesOwnerAwarenessAsDeclared(t *testing.T) {
	for declaration, want := range map[string]bool{"": false, "owner_aware: false\n": false, "owner_aware: true\n": true} {
		t.Run(strings.TrimSpace(declaration), func(t *testing.T) {
			doc := deliveredAuthorityDocument(t, writeContract(t, ownedContract+declaration))
			require.Equal(t, want, doc.GetOwnerAware())
			var user *compositionv1.RoleV1
			for _, role := range doc.GetRoles() {
				if role.GetName() == "user" {
					user = role
				}
			}
			require.NotNil(t, user, "the delivered document carries the user role")
			require.Len(t, user.GetScopes(), 1)
			require.Equal(t, "quill.page", user.GetScopes()[0].GetResourceKind())
			require.Equal(t, []string{"read"}, user.GetScopes()[0].GetActions())
			require.Len(t, user.GetInstallationOwnedScopes(), 1)
			require.Equal(t, "documents.collection", user.GetInstallationOwnedScopes()[0].GetResourceKind())
			require.Equal(t, []string{"read"}, user.GetInstallationOwnedScopes()[0].GetActions())
		})
	}
}

func TestOwnerAwareReadFailureStopsDelivery(t *testing.T) {
	dir := writeContract(t, ownedContract+"owner_aware: yes\n")
	d := NewCompositionDelivery(nil, nativeSigner(t), nil, nil, nil,
		func(context.Context, string) (bool, error) { return OwnerAware(dir, composed) }, nil)
	_, err := d.Prepare(t.Context(), "shop", false)
	require.ErrorContains(t, err, "owner_aware")
	for _, dir := range []string{"", t.TempDir()} {
		declared, err := OwnerAware(dir, composed)
		require.NoError(t, err)
		require.False(t, declared)
	}
}
