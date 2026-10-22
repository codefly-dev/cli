package environments

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// EnvironmentTelemetry is where the services of this environment export their
// operational telemetry: the OTLP intake the environment's infrastructure
// provides. On a hosted environment it is imported from the coordinate contract
// (the producer resolves it, as it resolves every other address); for `local`
// it names the developer's own collector. A service never declares it and an
// operator never types it into a configuration group: the CLI derives
// OTEL_EXPORTER_OTLP_ENDPOINT and its companions for every service it runs or
// renders (TelemetryCarriers), the same way it derives the self-endpoint
// carrier, and the SDK enables its exporter when they are present.
type EnvironmentTelemetry struct {
	// OTLPEndpoint is the OTLP/gRPC intake as host:port, or as a URL whose
	// scheme fixes the transport (http:// plaintext, https:// TLS), which is
	// what the OpenTelemetry specification admits for OTEL_EXPORTER_OTLP_ENDPOINT.
	OTLPEndpoint string `yaml:"otlp-endpoint"`
	// Insecure sends plaintext to a host:port endpoint. A URL's scheme already
	// says so, and the two are refused together when they disagree.
	Insecure bool `yaml:"insecure,omitempty"`
}

// Validate refuses an endpoint a service could not dial: empty, carrying
// credentials, a URL with a path, or plaintext asked of an https:// URL.
func (telemetry *EnvironmentTelemetry) Validate() error {
	if telemetry == nil {
		return nil
	}
	endpoint := strings.TrimSpace(telemetry.OTLPEndpoint)
	if endpoint == "" {
		return fmt.Errorf("telemetry declares no otlp-endpoint")
	}
	if strings.Contains(endpoint, "://") {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Host == "" {
			return fmt.Errorf("telemetry otlp-endpoint %q is not a URL a service can dial", endpoint)
		}
		if parsed.User != nil {
			return fmt.Errorf("telemetry otlp-endpoint %q carries credentials in its address", endpoint)
		}
		if parsed.Path != "" && parsed.Path != "/" {
			return fmt.Errorf("telemetry otlp-endpoint %q names a path; an OTLP intake is a host, the exporter adds the signal path", endpoint)
		}
		switch parsed.Scheme {
		case "http", "https":
		default:
			return fmt.Errorf("telemetry otlp-endpoint %q has scheme %q; only http and https are OTLP transports", endpoint, parsed.Scheme)
		}
		if telemetry.Insecure && parsed.Scheme == "https" {
			return fmt.Errorf("telemetry otlp-endpoint %q is https and insecure is set; one of them is wrong", endpoint)
		}
		return nil
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" {
		return fmt.Errorf("telemetry otlp-endpoint %q is not host:port", endpoint)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("telemetry otlp-endpoint %q has no valid port", endpoint)
	}
	return nil
}

// TelemetryCarriers is the process environment a service of this environment
// receives so its SDK exports operational telemetry: nil when the environment
// declares none. The endpoint is always delivered as a URL (see below). OTEL_SERVICE_NAME is the service's unique name in the
// composition and OTEL_RESOURCE_ATTRIBUTES names the module and the
// environment, so a trace read anywhere says which service of which composition
// in which environment produced it. The keys are the OpenTelemetry
// specification's, read by every SDK; nothing here is Codefly's own vocabulary.
// IsTelemetryCarrier reports whether key is one of the carriers
// TelemetryCarriers delivers. They are offered, never required: a process
// that loads no Codefly ConfigMap (a vendor image such as redis or postgres)
// has no SDK to export with and reads none of them, so a render must not
// refuse such a service for lacking a container to deliver them into.
func IsTelemetryCarrier(key string) bool {
	switch key {
	case "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES":
		return true
	}
	return false
}

func (env *Environment) TelemetryCarriers(module, service string) map[string]string {
	if env == nil || env.Telemetry == nil {
		return nil
	}
	endpoint := strings.TrimSpace(env.Telemetry.OTLPEndpoint)
	if endpoint == "" {
		return nil
	}
	// Delivered as a URL whatever shape it was declared in: the scheme is how
	// every OpenTelemetry SDK (Go's wool/otel, Python's, JavaScript's) learns
	// the transport, where a bare host:port is read as plaintext by one and as
	// TLS by another. The environment says which it meant with `insecure`.
	if !strings.Contains(endpoint, "://") {
		if env.Telemetry.Insecure {
			endpoint = "http://" + endpoint
		} else {
			endpoint = "https://" + endpoint
		}
	}
	return map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": endpoint,
		"OTEL_SERVICE_NAME":           module + "/" + service,
		"OTEL_RESOURCE_ATTRIBUTES":    "service.namespace=" + module + ",deployment.environment.name=" + env.Name,
	}
}
