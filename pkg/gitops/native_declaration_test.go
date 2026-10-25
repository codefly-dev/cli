package gitops

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// A native manifest lists only its own module's started identity workloads,
// each once.
func TestANativeManifestListsEachOfItsModulesWorkloadsOnce(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	deliverer := &NativeDeliverer{Workspace: &resources.Workspace{Name: "example"}, Env: nativeEnvironment()}
	input := &NativeWorkload{Module: module, Services: services, Started: nativeStarted("another", "api"), Carriers: nativeCarriers("crm")}
	_, err := deliverer.Render(t.Context(), input)
	require.ErrorContains(t, err, "does not declare module-identity in this module", "a matching service name in a different module is not listed")
	input.Started = nativeStarted("crm", "api", "api")
	_, err = deliverer.Render(t.Context(), input)
	require.ErrorContains(t, err, "more than once")
	input.Started = nil
	_, err = deliverer.Render(t.Context(), input)
	require.ErrorContains(t, err, "no started identity workload")
}
