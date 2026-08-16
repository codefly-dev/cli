package expose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/routing"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/spf13/cobra"
)

// ServiceCmd renders the edge routing manifests for a service's public
// endpoints. It turns the environment's ingress intent plus the deterministic
// in-cluster backend contract into concrete Gateway API (or legacy Istio)
// routes, so a solution's rendered output carries its own routing.
var ServiceCmd = &cobra.Command{
	Use:   "service [service]",
	Short: "Render edge routing manifests for a service's public endpoints",
	Long: `Service renders the Kubernetes routing manifests that publish a service's
public endpoints at the shared gateway. Hostnames come from the environment's
ingress intent (or --host); the in-cluster backend and port are resolved
deterministically, so nothing is guessed.

The default backend emits Gateway API GRPCRoute/HTTPRoute (implemented by Istio
when the gateway's class is istio); --routing istio emits the legacy
VirtualService envelope instead. Pass --prefix to scope gRPC routes to a proto
package.

Manifests print to stdout by default, or write to --output as one file per
service and environment so install/uninstall carries routing automatically.

Examples:
  codefly expose service accounts --host api.acme.dev --prefix acme.accounts.v1
  codefly expose service accounts --routing istio --output deployment/routes`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		cli.Init()

		backend := mustString(cmd, "routing")
		envName := mustString(cmd, "env")
		gatewayName := mustString(cmd, "gateway")
		gatewayNamespace := mustString(cmd, "gateway-namespace")
		prefix := strings.TrimSpace(mustString(cmd, "prefix"))
		hostOverride := mustStringSlice(cmd, "host")
		enableMTLS := mustBool(cmd, "mtls")
		output := strings.TrimSpace(mustString(cmd, "output"))

		workspace, module, service, err := common.LoadRequiredE(ctx, args)
		if err != nil {
			return fmt.Errorf("codefly expose service: %w", err)
		}

		env, err := orchestration.SelectEnvironment(workspace, envName)
		if err != nil {
			return fmt.Errorf("codefly expose service: %w", err)
		}
		if env.Namespace == "" {
			return fmt.Errorf("codefly expose service: environment %q declares no namespace", env.Name)
		}

		exposure := &routing.Exposure{
			Service:    service.Name,
			Namespace:  env.Namespace,
			Gateway:    routing.GatewayRef{Name: gatewayName, Namespace: gatewayNamespace},
			Endpoints:  exposedEndpoints(ctx, module.Name, service, env, hostOverride, prefix),
			EnableMTLS: enableMTLS,
		}

		manifests, err := routing.Render(backend, exposure)
		if err != nil {
			return fmt.Errorf("codefly expose service: %w", err)
		}

		if output == "" {
			fmt.Fprint(os.Stdout, manifests)
			return nil
		}
		if err := os.MkdirAll(output, 0o755); err != nil {
			return fmt.Errorf("codefly expose service: %w", err)
		}
		file := filepath.Join(output, fmt.Sprintf("%s.%s.routing.yaml", service.Name, env.Name))
		if err := os.WriteFile(file, []byte(manifests), 0o600); err != nil {
			return fmt.Errorf("codefly expose service: %w", err)
		}
		fmt.Fprintln(os.Stderr, file)
		return nil
	},
}

// exposedEndpoints builds the routable public endpoints of a service: their
// in-cluster ports, the ingress hosts bound to each endpoint, and (for gRPC)
// the proto-package prefix. TCP and unsupported endpoints are skipped — the
// edge cannot HTTP/gRPC-route them.
func exposedEndpoints(ctx context.Context, module string, service *resources.Service, env *resources.Environment, hostOverride []string, prefix string) []routing.ExposedEndpoint {
	ports := inClusterPorts(ctx, module, service.Name, service.Endpoints)
	var endpoints []routing.ExposedEndpoint
	for _, ep := range service.Endpoints {
		if ep.Visibility != resources.VisibilityPublic {
			continue
		}
		api := resolveAPI(ep)
		if standards.IsSupportedAPI(api) != nil || api == standards.TCP {
			continue
		}
		hosts := hostOverride
		if len(hosts) == 0 {
			hosts = ingressHosts(env, module, service.Name, ep.Name)
		}
		endpoint := routing.ExposedEndpoint{
			Name:  ep.Name,
			API:   api,
			Port:  ports[ep.Name],
			Hosts: hosts,
		}
		// The prefix is a gRPC proto package; it is meaningless as an HTTP path.
		if endpoint.GRPC() {
			endpoint.Prefix = prefix
		}
		endpoints = append(endpoints, endpoint)
	}
	return endpoints
}

// resolveAPI mirrors resources.Endpoint.Proto: fall the endpoint's name in as
// the API when the name is itself a supported API and none was declared.
func resolveAPI(ep *resources.Endpoint) string {
	api := ep.API
	if api == "" && standards.IsSupportedAPI(ep.Name) == nil {
		api = ep.Name
	}
	return api
}

// inClusterPorts resolves each endpoint's in-cluster Service port with the same
// rule as core network.RemoteManager.GenerateNetworkMappings: the canonical
// owner of a per-API port keeps standards.Port; every other endpoint (a named
// sibling, or a second API that hashes to the same canonical port) gets a
// stable endpoint-specific port. Using standards.Port for all of them would
// point a sibling's route at the wrong port. The computation runs over every
// non-external endpoint (not just the public ones) because a private endpoint
// can own the canonical port.
func inClusterPorts(ctx context.Context, module, service string, endpoints []*resources.Endpoint) map[string]uint16 {
	priority := make(map[string]int, len(standards.APIS()))
	for index, api := range standards.APIS() {
		priority[api] = index
	}

	type resolved struct {
		endpoint *resources.Endpoint
		api      string
		key      string
	}
	var considered []resolved
	counts := make(map[string]int)
	for _, ep := range endpoints {
		if ep.External() {
			continue
		}
		api := resolveAPI(ep)
		considered = append(considered, resolved{endpoint: ep, api: api, key: resources.ServiceUnique(module, service) + ep.Information().Identifier()})
		counts[api]++
	}

	owners := make(map[uint16]*resources.Endpoint)
	ownerKey := make(map[uint16]string)
	ownerAPI := make(map[uint16]string)
	for _, item := range considered {
		if counts[item.api] > 1 && item.endpoint.Name != item.api {
			continue
		}
		port := standards.Port(item.api)
		if _, ok := owners[port]; !ok ||
			priority[item.api] < priority[ownerAPI[port]] ||
			(priority[item.api] == priority[ownerAPI[port]] && item.key < ownerKey[port]) {
			owners[port] = item.endpoint
			ownerKey[port] = item.key
			ownerAPI[port] = item.api
		}
	}

	ports := make(map[string]uint16, len(considered))
	for _, item := range considered {
		port := standards.Port(item.api)
		if owners[port] != item.endpoint {
			port = network.ToNamedPort(ctx, "", module, service, item.endpoint.Name, item.api, network.PortModeHost)
		}
		ports[item.endpoint.Name] = port
	}
	return ports
}

// ingressHosts returns the hosts an environment ingress route binds to this
// endpoint. A route matches when it names the service (by bare name or
// module/service unique) and either names this endpoint or is service-wide
// (empty Endpoint). The per-endpoint Endpoint field is honored so a host meant
// for one endpoint is not applied to another.
func ingressHosts(env *resources.Environment, module, service, endpoint string) []string {
	unique := resources.ServiceUnique(module, service)
	var hosts []string
	seen := make(map[string]struct{})
	for _, route := range env.Ingress {
		if route.Service != service && route.Service != unique {
			continue
		}
		if route.Endpoint != "" && route.Endpoint != endpoint {
			continue
		}
		for _, host := range route.Hosts {
			if _, ok := seen[host]; ok {
				continue
			}
			seen[host] = struct{}{}
			hosts = append(hosts, host)
		}
	}
	return hosts
}

func init() {
	ServiceCmd.Flags().String("routing", "gateway-api", fmt.Sprintf("Routing backend (%s)", strings.Join(routing.Backends(), ", ")))
	ServiceCmd.Flags().String("env", "local", "Environment whose namespace and ingress hosts to render for")
	ServiceCmd.Flags().String("gateway", "codefly-gateway", "Shared gateway the routes attach to")
	ServiceCmd.Flags().String("gateway-namespace", "", "Namespace of the shared gateway (defaults to the service namespace)")
	ServiceCmd.Flags().String("prefix", "", "gRPC proto package to scope gRPC routes to (matched as <prefix>.*)")
	ServiceCmd.Flags().StringArray("host", nil, "Override the ingress hostnames (repeatable)")
	ServiceCmd.Flags().Bool("mtls", false, "Emit an Istio STRICT-mTLS PeerAuthentication (selector app=<service>); enable only when your workloads carry that label")
	ServiceCmd.Flags().String("output", "", "Write manifests to this directory instead of stdout")
}

func mustString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

func mustStringSlice(cmd *cobra.Command, name string) []string {
	v, _ := cmd.Flags().GetStringArray(name)
	return v
}

func mustBool(cmd *cobra.Command, name string) bool {
	v, _ := cmd.Flags().GetBool(name)
	return v
}
