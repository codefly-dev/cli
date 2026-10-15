package gitops

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// What withdraws a native delivery is the withdrawal a publication settles:
// the manifest's identity alone — no workload, endpoint, authority, frontend
// or group — removed, at the next generation, valid to core; and a withdrawal
// withdrawn again is itself, at the same generation.
func TestAWithdrawalIsTheManifestsIdentityAtTheNextGeneration(t *testing.T) {
	module, services := nativeModule(t, "crm", true)
	live, err := (&NativeDeliverer{Workspace: &resources.Workspace{Name: "example"}, Env: nativeEnvironment()}).Render(t.Context(), &NativeWorkload{
		Module: module, Services: services, Started: nativeStarted("crm", "api"), Carriers: nativeCarriers("crm"), FrontendBundle: &solutionhost.FrontendPayload{
			Artifact: FrontendArtifactName, Digest: solutionhost.PayloadDigest("sha256:" + "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"), MediaType: "application/x-tar",
		},
	})
	require.NoError(t, err)
	live.Generation = 3
	withdrawal, err := WithdrawalOf(live)
	require.NoError(t, err)
	require.Equal(t, &solutionhost.ModuleManifest{
		Schema: live.Schema, Binding: live.Binding, Alias: live.Alias, Kind: live.Kind, Generation: 4, Removed: true, Host: live.Host, Release: live.Release,
	}, withdrawal)
	require.NoError(t, withdrawal.Validate())
	require.False(t, live.Removed, "the delivered manifest is untouched")
	again, err := WithdrawalOf(withdrawal)
	require.NoError(t, err)
	require.Equal(t, withdrawal, again)
}
