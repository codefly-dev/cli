package orchestration

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	basev1 "github.com/codefly-dev/core/generated/go/codefly/base/v1"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

const wikiSelfKey = "CODEFLY__SELF_ENDPOINT__QUILL__BACKEND__HTTP__HTTP"

// ownMapping is one endpoint of the service being run, with its listen address
// (Native) and the address containers reach it at (Container).
func ownMapping(native, container string) *basev1.NetworkMapping {
	mapping := &basev1.NetworkMapping{
		Endpoint: &basev1.Endpoint{Module: "quill", Service: "backend", Name: "http", Api: "http"},
		Instances: []*basev1.NetworkInstance{
			{Access: resources.NewNativeNetworkAccess(), Address: native},
		},
	}
	if container != "" {
		mapping.Instances = append(mapping.Instances, &basev1.NetworkInstance{Access: resources.NewContainerNetworkAccess(), Address: container})
	}
	return mapping
}

// A render asks for container access: the in-cluster address, never the
// listen address.
func TestSelfEndpointCarriesTheRequestedAccessNotTheListenAddress(t *testing.T) {
	got := SelfEndpointEnvironmentVariables(t.Context(), []*basev1.NetworkMapping{
		ownMapping("localhost:8080", "http://backend.example-quill.svc.cluster.local:8080"),
	}, resources.NewContainerNetworkAccess())
	require.Equal(t, map[string]string{wikiSelfKey: "http://backend.example-quill.svc.cluster.local:8080"}, got)
}

// The key is core's: same normalization as the endpoint carrier, core's prefix.
func TestSelfEndpointUsesCoresCarrierName(t *testing.T) {
	mapping := ownMapping("localhost:9090", "api-gateway.example.svc.cluster.local:9090")
	mapping.Endpoint = &basev1.Endpoint{Module: "host-app", Service: "api-gateway", Name: "grpc", Api: "grpc"}
	got := SelfEndpointEnvironmentVariables(t.Context(), []*basev1.NetworkMapping{mapping}, resources.NewContainerNetworkAccess())
	key := resources.SelfEndpointAsEnvironmentVariableKey(resources.EndpointInformationFromProto(mapping.Endpoint))
	require.Equal(t, "CODEFLY__SELF_ENDPOINT__HOST_APP__API_GATEWAY__GRPC__GRPC", key)
	require.Contains(t, got, key)
}

func TestSelfEndpointIsNeverGuessedWithoutAMatchingInstance(t *testing.T) {
	require.Nil(t, SelfEndpointEnvironmentVariables(t.Context(),
		[]*basev1.NetworkMapping{ownMapping("localhost:8080", ""), nil}, resources.NewContainerNetworkAccess()))
}

// A run advertises the address matching its own runtime context, as core's
// RuntimeWrapper.AddSelfEndpoints does, and the carrier must already ride Init:
// an agent keeps the first non-empty override set it receives.
func TestRunnerOverridesCarryTheSelfEndpointAtInitAndStart(t *testing.T) {
	proposed := []*basev1.NetworkMapping{ownMapping("localhost:8080", "host.docker.internal:8080")}
	accepted := []*basev1.NetworkMapping{ownMapping("localhost:8081", "host.docker.internal:8081")}
	runner := &Runner{
		world:          &World{Env: &environments.Environment{}},
		overrides:      map[string]string{"APPLICATION_MODE": "dogfood"},
		runtimeContext: resources.RuntimeContextNative,
	}

	init := runner.runtimeOverridesFor(proposed)
	require.Equal(t, "localhost:8080", init[wikiSelfKey])
	require.Equal(t, "dogfood", init["APPLICATION_MODE"])

	runner.networkMappings = accepted
	require.Equal(t, "localhost:8081", runner.runtimeOverrides()[wikiSelfKey])

	runner.runtimeContext = resources.RuntimeContextContainer
	require.Equal(t, "host.docker.internal:8081", runner.runtimeOverrides()[wikiSelfKey])
}

func TestRunnerOverridesLetAnExplicitSetWinOverTheSelfEndpoint(t *testing.T) {
	runner := &Runner{
		world:           &World{Env: &environments.Environment{}},
		overrides:       map[string]string{wikiSelfKey: "https://quill.example.com"},
		networkMappings: []*basev1.NetworkMapping{ownMapping("localhost:8080", "host.docker.internal:8080")},
		runtimeContext:  resources.RuntimeContextContainer,
	}
	require.Equal(t, "https://quill.example.com", runner.runtimeOverrides()[wikiSelfKey])
}

// TestNativeRunDeliversTheEnvironmentsTelemetryCarriers pins that a native run
// hands every service the environment's telemetry carriers, derived like the
// self carrier and beneath an operator's --set, and nothing when the
// environment declares no telemetry.
func TestNativeRunDeliversTheEnvironmentsTelemetryCarriers(t *testing.T) {
	instance := &services.Instance{Module: &resources.Module{Name: "saas"}, Service: &resources.Service{Name: "accounts"}}
	runner := &Runner{
		instance:       instance,
		world:          &World{Env: &environments.Environment{Name: "local", Telemetry: &environments.EnvironmentTelemetry{OTLPEndpoint: "localhost:4317", Insecure: true}}},
		overrides:      map[string]string{"OTEL_SERVICE_NAME": "operator-named"},
		runtimeContext: resources.RuntimeContextNative,
	}
	got := runner.runtimeOverridesFor(nil)
	require.Equal(t, "http://localhost:4317", got["OTEL_EXPORTER_OTLP_ENDPOINT"])
	require.Equal(t, "deployment.environment.name=local", got["OTEL_RESOURCE_ATTRIBUTES"])
	require.Equal(t, "operator-named", got["OTEL_SERVICE_NAME"], "an explicit --set of the same key is the operator's to make")

	silent := &Runner{instance: instance, world: &World{Env: &environments.Environment{Name: "local"}}, runtimeContext: resources.RuntimeContextNative}
	require.NotContains(t, silent.runtimeOverridesFor(nil), "OTEL_EXPORTER_OTLP_ENDPOINT")
}
