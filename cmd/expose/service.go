package expose

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/routing"
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
VirtualService envelope instead. Pass --prefix to scope the routes to a proto
package (gRPC) or URL path prefix (HTTP).

Manifests print to stdout by default, or write to --output as one file per
service so install/uninstall carries routing automatically.

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

		workspace, _, service, err := common.LoadRequiredE(ctx, args)
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

		hosts := hostOverride
		if len(hosts) == 0 {
			hosts = ingressHosts(env, service.Name)
		}

		exposure := routing.Exposure{
			Service:    service.Name,
			Namespace:  env.Namespace,
			Hosts:      hosts,
			Prefix:     prefix,
			Gateway:    routing.GatewayRef{Name: gatewayName, Namespace: gatewayNamespace},
			Endpoints:  publicEndpoints(service),
			EnableMTLS: enableMTLS,
		}

		manifests, err := routing.Render(backend, exposure)
		if err != nil {
			return fmt.Errorf("codefly expose service: %w", err)
		}

		if len(hosts) == 0 {
			fmt.Fprintf(os.Stderr, "warning: no ingress hosts for %s in environment %q; routes will match every gateway hostname\n", service.Name, env.Name)
		}

		if output == "" {
			fmt.Fprint(os.Stdout, manifests)
			return nil
		}
		if err := os.MkdirAll(output, 0o755); err != nil {
			return fmt.Errorf("codefly expose service: %w", err)
		}
		file := filepath.Join(output, fmt.Sprintf("%s.routing.yaml", service.Name))
		if err := os.WriteFile(file, []byte(manifests), 0o644); err != nil {
			return fmt.Errorf("codefly expose service: %w", err)
		}
		fmt.Fprintln(os.Stderr, file)
		return nil
	},
}

func publicEndpoints(service *resources.Service) []routing.ExposedEndpoint {
	var endpoints []routing.ExposedEndpoint
	for _, ep := range service.Endpoints {
		if ep.Visibility != resources.VisibilityPublic {
			continue
		}
		api := ep.API
		if api == "" && standards.IsSupportedAPI(ep.Name) == nil {
			api = ep.Name
		}
		if standards.IsSupportedAPI(api) != nil || api == standards.TCP {
			continue
		}
		endpoints = append(endpoints, routing.ExposedEndpoint{
			Name: ep.Name,
			API:  api,
			Port: standards.Port(api),
		})
	}
	return endpoints
}

func ingressHosts(env *resources.Environment, service string) []string {
	var hosts []string
	seen := make(map[string]struct{})
	for _, route := range env.Ingress {
		if route.Service != service {
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
	ServiceCmd.Flags().String("prefix", "", "Path-prefix contract: proto package (gRPC) or URL path prefix (HTTP)")
	ServiceCmd.Flags().StringArray("host", nil, "Override the ingress hostnames (repeatable)")
	ServiceCmd.Flags().Bool("mtls", true, "Emit an Istio STRICT-mTLS PeerAuthentication for the backend")
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
