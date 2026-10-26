package gitops

import (
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/kustomize/kyaml/openapi"
)

// clusterScopedKyamlForgets exists only while the pinned kyaml has no answer
// for these kinds. Once it has one, kyaml's answer is used and the table is
// dead: delete it, and this test with it.
func TestKyamlStillForgetsTheAdmissionPolicyScopes(t *testing.T) {
	for typeMeta := range clusterScopedKyamlForgets {
		_, known := openapi.IsNamespaceScoped(typeMeta)
		require.False(t, known, "kyaml now knows the scope of %s: delete clusterScopedKyamlForgets", typeMeta.Kind)
	}
}

// An admission policy reaches every namespace, so it needs the AppProject's
// cluster authority whatever the pinned kyaml knows about it.
func TestAdmissionPoliciesNeedClusterAuthority(t *testing.T) {
	for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
		t.Run(kind, func(t *testing.T) {
			manifests, _, err := decodeYAML("policy.yaml", []byte("apiVersion: admissionregistration.k8s.io/v1\nkind: "+kind+"\nmetadata:\n  name: shared\n  namespace: alpha\n"))
			require.NoError(t, err)
			require.Len(t, manifests, 1)
			require.True(t, manifests[0].knownClusterScoped())
			require.ErrorContains(t, validateManifest(manifests[0], nil, true), "cluster-scoped "+kind+" is outside an AppProject contract")
		})
	}
}
