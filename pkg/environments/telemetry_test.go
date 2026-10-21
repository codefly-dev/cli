package environments

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTelemetryRefusesWhatAServiceCannotDial pins the shapes an OTLP intake
// may take and the ones it may not: an address a service would fail to reach,
// or one that would leak a credential into every pod's environment.
func TestTelemetryRefusesWhatAServiceCannotDial(t *testing.T) {
	for name, telemetry := range map[string]*EnvironmentTelemetry{
		"host:port":         {OTLPEndpoint: "otel.internal.ops.example:443"},
		"insecure host":     {OTLPEndpoint: "localhost:4317", Insecure: true},
		"https url":         {OTLPEndpoint: "https://otel.internal.ops.example:443"},
		"http url":          {OTLPEndpoint: "http://collector.observability.svc:4318"},
		"url with slash":    {OTLPEndpoint: "https://otel.example/"},
		"nil declares none": nil,
	} {
		require.NoError(t, telemetry.Validate(), name)
	}
	for name, telemetry := range map[string]*EnvironmentTelemetry{
		"empty":              {},
		"no port":            {OTLPEndpoint: "otel.example"},
		"bad port":           {OTLPEndpoint: "otel.example:http"},
		"credentials":        {OTLPEndpoint: "https://user:pass@otel.example:443"},
		"path":               {OTLPEndpoint: "https://otel.example:443/v1/traces"},
		"grpc scheme":        {OTLPEndpoint: "grpc://otel.example:4317"},
		"https and insecure": {OTLPEndpoint: "https://otel.example:443", Insecure: true},
	} {
		require.Error(t, telemetry.Validate(), name)
	}
	env := &Environment{Name: "staging", Telemetry: &EnvironmentTelemetry{}}
	require.ErrorContains(t, env.Validate(), "environment staging: telemetry declares no otlp-endpoint")
}

// TestTelemetryCarriersNameTheServiceAndItsEnvironment pins what a service
// receives: the specification's own variables, the service's unique name, and
// resource attributes naming its module and environment — and nothing at all
// from an environment that declares no telemetry.
func TestTelemetryCarriersNameTheServiceAndItsEnvironment(t *testing.T) {
	env := &Environment{Name: "staging", Telemetry: &EnvironmentTelemetry{OTLPEndpoint: "otel.internal.ops.example:443"}}
	require.Equal(t, map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "https://otel.internal.ops.example:443",
		"OTEL_SERVICE_NAME":           "saas/accounts",
		"OTEL_RESOURCE_ATTRIBUTES":    "service.namespace=saas,deployment.environment.name=staging",
	}, env.TelemetryCarriers("saas", "accounts"), "a bare host:port is delivered as the URL its transport implies, so every SDK reads the same thing")

	local := &Environment{Name: "local", Telemetry: &EnvironmentTelemetry{OTLPEndpoint: "localhost:4317", Insecure: true}}
	require.Equal(t, "http://localhost:4317", local.TelemetryCarriers("saas", "accounts")["OTEL_EXPORTER_OTLP_ENDPOINT"])
	asURL := &Environment{Name: "staging", Telemetry: &EnvironmentTelemetry{OTLPEndpoint: "http://collector.observability.svc:4318"}}
	require.Equal(t, "http://collector.observability.svc:4318", asURL.TelemetryCarriers("saas", "accounts")["OTEL_EXPORTER_OTLP_ENDPOINT"], "a URL is delivered as written")

	require.Nil(t, (&Environment{Name: "local"}).TelemetryCarriers("saas", "accounts"))
	var none *Environment
	require.Nil(t, none.TelemetryCarriers("saas", "accounts"))
}

// TestCoordinateContractCarriesTelemetry pins that a producer's contract can
// state the intake and that an import keeps it: the address is the producer's
// to resolve, like every other address in the contract.
func TestCoordinateContractCarriesTelemetry(t *testing.T) {
	contract, err := ParseCoordinateContract([]byte(`{"schema":"codefly/coordinate/v1","coordinate":"region-a","environment":{"name":"staging","telemetry":{"otlp-endpoint":"otel.internal.ops.example:443"}}}`))
	require.NoError(t, err)
	require.Equal(t, "otel.internal.ops.example:443", contract.Environment.Telemetry.OTLPEndpoint)
	require.NoError(t, contract.Environment.Telemetry.Validate())
}
