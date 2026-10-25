package gitops

import (
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

func TestNativeDeclarationAdmitsWorkloadsBeforePublishingTheirRoutes(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	deliverer := &NativeDeliverer{Workspace: &resources.Workspace{Name: "example"}, Env: nativeEnvironment()}
	input := &NativeWorkload{Module: module, Services: services, Declared: nativeStarted("crm", "api", "worker"), Carriers: nativeCarriers("crm")}
	early, err := deliverer.Render(t.Context(), input)
	require.NoError(t, err)
	require.Len(t, early.Workloads, 2)
	require.Empty(t, early.Endpoints)
	require.NotNil(t, early.Authority)
	input.Started = nativeStarted("crm", "api")
	ready, err := deliverer.Render(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, early.Workloads, ready.Workloads)
	require.Len(t, ready.Endpoints, 1)
	require.Equal(t, "api", ready.Endpoints[0].Service)
	input.Declared = nativeStarted("crm", "worker")
	_, err = deliverer.Render(t.Context(), input)
	require.ErrorContains(t, err, "outside its declared membership")
	input.Started = nil
	input.Declared = nativeStarted("another", "api")
	_, err = deliverer.Render(t.Context(), input)
	require.Error(t, err, "a matching service name in a different module is not admitted")
	input.Declared = nativeStarted("crm", "api", "api")
	_, err = deliverer.Render(t.Context(), input)
	require.ErrorContains(t, err, "more than once")
}

func TestNativeFrontendDeferralIsExplicitAndDoesNotWeakenPublication(t *testing.T) {
	module, services := nativeModule(t, "crm", true)
	deliverer := &NativeDeliverer{Workspace: &resources.Workspace{Name: "example"}, Env: nativeEnvironment()}
	input := &NativeWorkload{Module: module, Services: services, Declared: nativeStarted("crm", "api"), DeferFrontend: true}
	early, err := deliverer.Render(t.Context(), input)
	require.NoError(t, err)
	require.Nil(t, early.Frontend, "a deferred page must not claim to carry a payload")
	input.DeferFrontend = false
	_, err = deliverer.Render(t.Context(), input)
	require.ErrorContains(t, err, "built no bundle")
	input.FrontendBundle = &solutionhost.FrontendPayload{Artifact: FrontendArtifactName, Digest: solutionhost.PayloadDigest("sha256:" + strings.Repeat("e", 64)), MediaType: "application/x-tar"}
	ready, err := deliverer.Render(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, early.Workloads, ready.Workloads)
	require.Equal(t, input.FrontendBundle, ready.Frontend.Payload)
	input.DeferFrontend = true
	_, err = deliverer.Render(t.Context(), input)
	require.ErrorContains(t, err, "both defer and publish")
}
