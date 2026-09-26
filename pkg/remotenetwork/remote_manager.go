package remotenetwork

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/codefly-dev/cli/pkg/environments"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	corenetwork "github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/wool"
)

type RemoteManager struct {
	dnsManager corenetwork.DNSManager

	// pairingsWG tracks the goroutines spawned by StartPairing
	// (port-forward + log fetch). Stop() blocks on it so callers can
	// guarantee the goroutines (and their kubectl child processes) are
	// torn down before they continue. Without this the goroutines were
	// fire-and-forget and outlived the calling context.
	pairingsWG sync.WaitGroup
}

// Stop blocks until every pairing goroutine has exited. The caller is
// expected to have cancelled the context passed to StartPairing first
// (or all pairing contexts are children of one that just got cancelled).
// Safe to call multiple times.
func (m *RemoteManager) Stop() {
	m.pairingsWG.Wait()
}

// GetNamespace is the namespace a service's workloads bind to and are addressed
// in. A declared environment namespace resolves per module through
// Environment.ModuleNamespace, from the service's own module: the address
// synthesized for a dependency in another module therefore names the provider's
// namespace, never the consumer's. Undeclared, a per-module namespace is
// synthesized from the workspace, module and environment names.
func (m *RemoteManager) GetNamespace(_ context.Context, env *environments.Environment, workspace *resources.Workspace, service *resources.ServiceIdentity) (string, error) {
	if namespace := env.ModuleNamespace(workspace, service.Module); namespace != "" {
		return namespace, nil
	}

	if workspace.Layout == resources.LayoutKindFlat {
		return fmt.Sprintf("%s-%s", workspace.Name, env.Name), nil
	}
	return fmt.Sprintf("%s-%s-%s", workspace.Name, service.Module, env.Name), nil
}

func (m *RemoteManager) KubernetesService(service *resources.ServiceIdentity, endpoint *basev0.Endpoint, namespace string, port uint16) *basev0.NetworkInstance {
	host := fmt.Sprintf("%s.%s.svc.cluster.local", service.Name, namespace)
	var instance *basev0.NetworkInstance
	if standards.IsHTTPBasedAPI(endpoint.Api) {
		instance = resources.NewHTTPNetworkInstance(host, port, resources.EndpointSecured(endpoint))
	} else {
		instance = resources.NewNetworkInstance(host, port)
	}
	instance.Access = resources.NewContainerNetworkAccess()
	return instance
}

// GenerateNetworkMappings generates network mappings for a service endpoints.
//
// Unlike RuntimeManager, this takes no runtime context: remote (k8s) ports are
// core's network.DeployedEndpointPorts over the in-cluster endpoints. The
// conventional endpoint of the highest-priority API keeps a shared canonical
// port; named siblings and colliding APIs receive stable endpoint-specific
// ports.
func (m *RemoteManager) GenerateNetworkMappings(ctx context.Context,
	env *environments.Environment,
	workspace *resources.Workspace,
	service *resources.ServiceIdentity,
	endpoints []*basev0.Endpoint) ([]*basev0.NetworkMapping, error) {
	w := wool.Get(ctx).In("network.Runtime.GenerateNetworkMappings")
	if m.dnsManager == nil {
		return nil, w.NewError("RemoteManager: dnsManager is nil — call NewRemoteManager with a non-nil DNSManager")
	}
	externalDNS, err := m.resolveExternalDNS(ctx, env, service, endpoints)
	if err != nil {
		return nil, err
	}
	ports, err := corenetwork.DeployedEndpointPorts(ctx, service.Module, service.Name, inClusterEndpoints(endpoints, externalDNS))
	if err != nil {
		return nil, w.Wrap(err)
	}
	var out []*basev0.NetworkMapping
	for _, endpoint := range endpoints {
		if endpoint == nil {
			return nil, w.NewError("cannot generate network mapping for nil endpoint")
		}
		nm := &basev0.NetworkMapping{
			Endpoint: endpoint,
		}
		if dns := externalDNS[endpoint]; dns != nil {
			nm.Instances = []*basev0.NetworkInstance{
				corenetwork.ExternalInstance(corenetwork.DNS(service, endpoint, dns)),
			}
			out = append(out, nm)
			continue
		}

		// In-cluster endpoints — internal ones, and external ones with no public
		// host that this flow renders as a workload anyway — use a declared
		// environment DNS contract when present. Otherwise Kubernetes service
		// discovery is synthesized.
		dns, dnsErr := m.dnsManager.GetDNS(ctx, service, endpoint.Name)
		if dnsErr == nil && dns != nil {
			nm.Instances = []*basev0.NetworkInstance{
				corenetwork.PublicInstance(corenetwork.DNS(service, endpoint, dns)),
				corenetwork.ContainerInstance(corenetwork.DNS(service, endpoint, dns)),
			}
			out = append(out, nm)
			continue
		}

		namespace, nsErr := m.GetNamespace(ctx, env, workspace, service)
		if nsErr != nil {
			return nil, nsErr
		}
		port := ports[endpoint.Name]
		// A no-ingress endpoint's ClusterIP is the only address that exists, so
		// it has to answer both Public and Container lookups — mirror the DNS
		// branch above. KubernetesService is called twice on purpose: it returns
		// a fresh instance each time and PublicInstance/ContainerInstance stamp
		// Access in place, so a single shared instance would collapse to one
		// access and silently drop the other.
		nm.Instances = append(nm.Instances,
			corenetwork.PublicInstance(m.KubernetesService(service, endpoint, namespace, port)),
			corenetwork.ContainerInstance(m.KubernetesService(service, endpoint, namespace, port)),
		)
		out = append(out, nm)
	}
	return out, nil
}

// DeployedPorts is the in-cluster port of each of a service's endpoints,
// keyed by endpoint name: the one allocation GenerateNetworkMappings renders,
// from core's network.DeployedEndpointPorts. An external endpoint that resolves
// to a public host has no cluster port and is absent.
func (m *RemoteManager) DeployedPorts(ctx context.Context,
	env *environments.Environment,
	service *resources.ServiceIdentity,
	endpoints []*basev0.Endpoint) (map[string]uint16, error) {
	w := wool.Get(ctx).In("network.Runtime.DeployedPorts")
	if m.dnsManager == nil {
		return nil, w.NewError("RemoteManager: dnsManager is nil — call NewRemoteManager with a non-nil DNSManager")
	}
	externalDNS, err := m.resolveExternalDNS(ctx, env, service, endpoints)
	if err != nil {
		return nil, err
	}
	ports, err := corenetwork.DeployedEndpointPorts(ctx, service.Module, service.Name, inClusterEndpoints(endpoints, externalDNS))
	if err != nil {
		return nil, w.Wrap(err)
	}
	return ports, nil
}

// resolveExternalDNS resolves every external endpoint to its public host.
func (m *RemoteManager) resolveExternalDNS(ctx context.Context,
	env *environments.Environment,
	service *resources.ServiceIdentity,
	endpoints []*basev0.Endpoint) (map[*basev0.Endpoint]*basev0.DNS, error) {
	// External endpoints resolve to an environment-specific public host: a
	// declared dns.codefly.yaml entry wins; otherwise the host is derived from
	// the environment's declared app host suffix (sourced from the coordinate
	// contract) so a promotable render is value-free. Resolution runs before
	// port allocation because an external endpoint with no public host that this
	// flow still renders in-cluster competes for the canonical port exactly like
	// an internal endpoint does.
	w := wool.Get(ctx).In("network.Runtime.resolveExternalDNS")
	externalDNS := make(map[*basev0.Endpoint]*basev0.DNS)
	for _, endpoint := range endpoints {
		if endpoint == nil || !resources.IsExternalEndpoint(endpoint) {
			continue
		}
		dns, err := m.dnsManager.GetDNS(ctx, service, endpoint.Name)
		if err == nil && dns != nil {
			externalDNS[endpoint] = dns
			continue
		}
		if derived := DerivedExternalDNS(env, service, endpoint); derived != nil {
			externalDNS[endpoint] = derived
			continue
		}
		if !renderedInCluster(env, service) {
			if err != nil {
				return nil, err
			}
			return nil, w.NewError("cannot find dns for endpoint %s", endpoint.Name)
		}
	}
	return externalDNS, nil
}

// inClusterEndpoints is the endpoints the render emits in-cluster: internal
// ones, and external ones with no public host that the flow renders as a
// workload anyway.
func inClusterEndpoints(endpoints []*basev0.Endpoint, externalDNS map[*basev0.Endpoint]*basev0.DNS) []*basev0.Endpoint {
	inCluster := make([]*basev0.Endpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint != nil && (!resources.IsExternalEndpoint(endpoint) || externalDNS[endpoint] == nil) {
			inCluster = append(inCluster, endpoint)
		}
	}
	return inCluster
}

// renderedInCluster reports whether the flow that asked for these mappings
// emits the service as an in-cluster workload (Deployment + Service), so an
// external endpoint with no declared or derivable public host still has an
// address to route to: the ClusterIP the render creates. The manager is only
// ever asked about services the flow deploys; the one class it deploys without
// an in-cluster workload is the environment's managed services (an external
// database, say), whose bundle is bootstrap-only and whose address must be
// declared. For those, and for a nil environment, nothing is synthesized and
// the caller keeps the hard failure.
func renderedInCluster(env *environments.Environment, service *resources.ServiceIdentity) bool {
	if env == nil || service == nil {
		return false
	}
	_, managed := env.ManagedService(service.Module, service.Name)
	return !managed
}

// DerivedExternalDNS builds a public DNS record for an external endpoint from
// the environment's declared app host suffix, or nil when no suffix is declared
// (the caller then falls back to a declared dns.codefly.yaml, or fails). The
// host is Environment.AppHost(service), reached over TLS on 443 — the public app
// edge (REST/HTTP/gRPC/Connect/MCP) terminates TLS there. A raw TCP external has
// no app-edge host, so it is not derived and still requires a declared DNS entry
// (mirrors the routable-endpoint rule in cmd/expose).
func DerivedExternalDNS(env *environments.Environment, service *resources.ServiceIdentity, endpoint *basev0.Endpoint) *basev0.DNS {
	if endpoint == nil || standards.IsSupportedAPI(endpoint.Api) != nil || endpoint.Api == standards.TCP {
		return nil
	}
	host := env.AppHost(service)
	if host == "" {
		return nil
	}
	return &basev0.DNS{
		Module:   service.Module,
		Service:  service.Name,
		Endpoint: endpoint.Name,
		Host:     host,
		Port:     443,
		Secured:  true,
	}
}

type Pairing struct {
	Local  *basev0.NetworkMapping
	Remote *basev0.NetworkMapping
}

func (m *RemoteManager) Expose(ctx context.Context,
	env *environments.Environment,
	workspace *resources.Workspace,
	service *resources.ServiceIdentity,
	endpoints []*basev0.Endpoint,
	localNetworkMappings []*basev0.NetworkMapping,
	output wool.LogProcessorWithSource) error {
	w := wool.Get(ctx).In("expose")
	remotes, err := m.GenerateNetworkMappings(ctx, env, workspace, service, endpoints)
	if err != nil {
		return w.Wrapf(err, "can't generate remote mappings")
	}
	var pairings []*Pairing
	for _, mapping := range localNetworkMappings {
		// Find the equivalent remote network mapping
		var remoteMapping *basev0.NetworkMapping
		for _, r := range remotes {
			if r.Endpoint.Module == mapping.Endpoint.Module &&
				r.Endpoint.Service == mapping.Endpoint.Service &&
				r.Endpoint.Name == mapping.Endpoint.Name &&
				r.Endpoint.Api == mapping.Endpoint.Api {
				remoteMapping = r
				break
			}
		}
		if remoteMapping == nil {
			return w.NewError("cannot find remote network mapping for local mapping")
		}
		pairings = append(pairings, &Pairing{Local: mapping, Remote: remoteMapping})
	}
	for _, pairing := range pairings {
		err = m.StartPairing(ctx, env, workspace, service, pairing, output)
		if err != nil {
			return w.Wrap(err)
		}
	}
	return nil
}

func (m *RemoteManager) StartPairing(ctx context.Context, _ *environments.Environment, _ *resources.Workspace, service *resources.ServiceIdentity, pairing *Pairing, output wool.LogProcessorWithSource) error {
	w := wool.Get(ctx).In("startPairing")
	// Internal endpoints carry both a Public and a Container instance off the
	// same ClusterIP host:port; port-forward the Container one. External
	// endpoints have only a Public instance. A count check here would reject
	// every internal mapping.
	remote := resources.FilterNetworkInstance(ctx, pairing.Remote.Instances, resources.NewContainerNetworkAccess())
	if remote == nil {
		remote = resources.FilterNetworkInstance(ctx, pairing.Remote.Instances, resources.NewPublicNetworkAccess())
	}
	if remote == nil {
		return w.NewError("no container or public instance in remote network mapping")
	}
	remotePort := remote.Port
	if remotePort > 65535 {
		return w.NewError("remote port %d exceeds 65535", remotePort)
	}
	remoteService, err := m.GetKubernetesService(ctx, service, remote.Hostname, uint16(remotePort))
	if err != nil {
		return w.Wrap(err)
	}
	// Find the native instances
	local := resources.FilterNetworkInstance(ctx, pairing.Local.Instances, resources.NewNativeNetworkAccess())
	if local == nil {
		return w.NewError("no native instance found in local network mapping")
	}
	localPort := local.Port
	if localPort > 65535 {
		return w.NewError("local port %d exceeds 65535", localPort)
	}
	forwardPort := uint16(localPort)
	// Each goroutine gets its own err binding — the previous version
	// shared the outer `err` between two parallel writers, which is a
	// data race. The WaitGroup lets Stop() block until both kubectl
	// child processes are reaped.
	m.pairingsWG.Add(2)
	go func() {
		defer m.pairingsWG.Done()
		if err := portForwardService(ctx, remoteService, forwardPort); err != nil {
			w.Warn(err.Error())
		}
	}()
	go func() {
		defer m.pairingsWG.Done()
		if err := fetchLogs(ctx, remoteService, output); err != nil {
			w.Warn(err.Error())
		}
	}()

	return nil
}

func (m *RemoteManager) GetKubernetesService(ctx context.Context, identity *resources.ServiceIdentity, hostname string, port uint16) (*KubernetesService, error) {
	w := wool.Get(ctx).In("getKubernetesService")
	// Parse: backend.codefly-platform-customers-local.svc.cluster.local

	hostParts := strings.Split(hostname, ".")
	if len(hostParts) < 3 {
		return nil, w.NewError("invalid host format: %s", hostname)
	}

	name := hostParts[0]
	namespace := hostParts[1]

	return &KubernetesService{
		Namespace:       namespace,
		Name:            name,
		Port:            port,
		ServiceIdentity: identity,
	}, nil
}

type KubernetesService struct {
	Namespace string
	Name      string
	Port      uint16
	*resources.ServiceIdentity
}

//nolint:gosec // G204: fixed kubectl executable, separate namespace/service arguments and numeric ports; no shell.
func portForwardService(ctx context.Context, k8sSvc *KubernetesService, localPort uint16) error {
	w := wool.Get(ctx).In("portForwardService")
	cmd := exec.CommandContext(ctx, "kubectl", "port-forward", "-n", k8sSvc.Namespace, fmt.Sprintf("svc/%s", k8sSvc.Name), fmt.Sprintf("%d:%d", localPort, k8sSvc.Port))
	w.Info("port-forward", wool.Field("cmd", cmd.Args))
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(err.Error(), "signal: killed") {
			return nil
		}
		if strings.Contains(err.Error(), "context canceled") {
			return nil
		}
		return w.NewError("Failed to forward service: %s, %s, error: %v, out: %s", k8sSvc.Unique(), cmd.Args, err, out)
	}
	return nil
}

//nolint:gosec // G204: fixed kubectl executable with namespace/service passed as separate arguments; no shell.
func fetchLogs(ctx context.Context, k8sService *KubernetesService, output wool.LogProcessorWithSource) error {
	w := wool.Get(ctx).In("fetchLogs").With(wool.Field("namespace", k8sService.Namespace), wool.ThisField(k8sService))
	identifier := &wool.Identifier{Unique: k8sService.Unique(), Kind: "SERVICE"}
	logsCmd := exec.CommandContext(ctx, "kubectl", "logs", "-n", k8sService.Namespace, fmt.Sprintf("svc/%s", k8sService.Name))
	w.Info("forwarding logs", wool.Field("cmd", logsCmd.Args))
	stdout, err := logsCmd.StdoutPipe()
	if err != nil {
		return w.Wrapf(err, "error creating StdoutPipe")
	}

	err = logsCmd.Start()
	if err != nil {
		return w.Wrapf(err, "error starting logs command for k8sService")
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		text := scanner.Text()
		if strings.Contains(text, "failed to try resolving symlinks in path") {
			continue // Ignore this specific error message
		}
		output.ProcessWithSource(identifier, &wool.Log{Message: text, Level: wool.FORWARD})
	}

	if err = scanner.Err(); err != nil {
		return w.Wrapf(err, "error scanning logs for k8sService")
	}
	err = logsCmd.Wait()
	if err != nil {
		return w.Wrapf(err, "error waiting for logs command for k8sService")
	}
	return nil
}

func NewRemoteManager(_ context.Context, dnsManager corenetwork.DNSManager) (*RemoteManager, error) {
	return &RemoteManager{dnsManager: dnsManager}, nil
}
