package orchestration

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/codefly-dev/core/architecture"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/wool"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// ReadinessPredicate names the check a readiness requirement applies.
type ReadinessPredicate string

const (
	// PredicateRequirements covers a failure to compute what the run requires
	// at all (an unresolvable dependency graph or service).
	PredicateRequirements ReadinessPredicate = "requirements"
	// PredicateLifecycle requires a service's runtime Start to have completed
	// successfully. An endpointless dependency (a one-shot job) is satisfied by
	// this predicate alone: having no endpoint is not evidence of readiness.
	PredicateLifecycle ReadinessPredicate = "lifecycle"
	// PredicateRunner revokes readiness once a started runner reports failure.
	PredicateRunner ReadinessPredicate = "runner"
	// PredicateMapping requires a consumed endpoint to have a recorded network
	// mapping with an address to probe.
	PredicateMapping ReadinessPredicate = "endpoint-mapping"
	// PredicateTransport is the legacy transport-only check: the endpoint
	// accepts a TCP connection. It is what an endpoint declaring neither gRPC
	// nor HTTP offers.
	PredicateTransport ReadinessPredicate = "tcp-connect"
	// PredicateGRPCHealth requires the gRPC health service to report SERVING. A
	// server that answers without implementing Health declares transport-only
	// readiness and is accepted as such — Health is never assumed on an
	// arbitrary server.
	PredicateGRPCHealth ReadinessPredicate = "grpc-health"
	// PredicateHTTPStatus requires the endpoint's declared route to answer with
	// a successful status. No health route is ever invented: the probe asks for
	// the endpoint's own address.
	PredicateHTTPStatus ReadinessPredicate = "http-status"
)

const (
	// Probes are polled (every 150ms by the run loop), so they are budgeted to
	// answer rather than to wait out a slow start: a service that has not
	// answered yet is simply not ready yet, and the next poll asks again.
	readinessTransportTimeout = 500 * time.Millisecond
	readinessProbeTimeout     = time.Second
	readinessProbeConcurrency = 8
)

var readinessHTTPClient = &http.Client{}

// ReadinessFailure identifies the requirement keeping a flow from being ready:
// the service, the endpoint (empty for service-wide requirements), the
// predicate that did not hold and the last error it produced.
type ReadinessFailure struct {
	Service   string
	Endpoint  string
	Predicate ReadinessPredicate
	Reason    string
}

func (failure *ReadinessFailure) String() string {
	target := failure.Service
	if failure.Endpoint != "" {
		target = fmt.Sprintf("%s endpoint %s", failure.Service, failure.Endpoint)
	}
	if target == "" {
		target = "flow"
	}
	if failure.Reason == "" {
		return fmt.Sprintf("%s: %s", target, failure.Predicate)
	}
	return fmt.Sprintf("%s: %s: %s", target, failure.Predicate, failure.Reason)
}

// readinessRequirement is a single predicate the run must satisfy. Lifecycle
// and mapping requirements are decided from recorded state; the others carry
// the address of the endpoint to probe.
type readinessRequirement struct {
	service   string
	endpoint  string
	predicate ReadinessPredicate
	address   string
	url       string
	secure    bool
	// reason carries a failure already established while planning, for
	// requirements that cannot be probed at all.
	reason string
}

func (requirement *readinessRequirement) failure(reason string) *ReadinessFailure {
	return &ReadinessFailure{
		Service:   requirement.service,
		Endpoint:  requirement.endpoint,
		Predicate: requirement.predicate,
		Reason:    reason,
	}
}

// Ready reports whether every readiness requirement of the run holds.
func (flow *Flow) Ready(ctx context.Context) bool {
	return flow.Readiness(ctx) == nil
}

// Readiness evaluates what this run must satisfy to be ready and returns the
// first requirement that does not hold, or nil when the flow is ready. The
// requirements are the completed lifecycle of every service the run is
// responsible for, plus a protocol-specific probe of every endpoint its
// consumers actually declare.
func (flow *Flow) Readiness(ctx context.Context) *ReadinessFailure {
	if flow == nil || flow.playbook == nil {
		return &ReadinessFailure{Predicate: PredicateLifecycle, Reason: "flow has not started"}
	}
	if failure, ok := flow.Failure(); ok {
		return &ReadinessFailure{Service: failure.Service, Predicate: PredicateRunner, Reason: failure.Message}
	}
	requirements, err := flow.readinessRequirements(ctx)
	if err != nil {
		return &ReadinessFailure{Predicate: PredicateRequirements, Reason: err.Error()}
	}
	// Settle every lifecycle before probing anything: a service that has not
	// finished starting must never be probed, or a leftover process on its
	// deterministically hashed port would answer for it.
	var probes []readinessRequirement
	for _, requirement := range requirements {
		if requirement.predicate != PredicateLifecycle {
			probes = append(probes, requirement)
			continue
		}
		if !flow.startedRun(requirement.service) {
			return requirement.failure("runtime start has not completed")
		}
	}
	return evaluateReadinessProbes(ctx, probes)
}

func (flow *Flow) readinessRequirements(ctx context.Context) ([]readinessRequirement, error) {
	origin := resources.WithUnique(flow.originService).Unique()
	var requirements []readinessRequirement
	if !flow.standAlone && flow.world != nil && flow.world.Dependencies != nil {
		order, err := flow.world.Dependencies.OrderTo(ctx, origin)
		if err != nil {
			return nil, err
		}
		consumers, err := flow.dependencyConsumers(order)
		if err != nil {
			return nil, err
		}
		for _, service := range order {
			requirements = append(requirements, readinessRequirement{service: service.Unique, predicate: PredicateLifecycle})
			endpoints, err := flow.endpointRequirements(service.Unique, consumers[service.Unique])
			if err != nil {
				return nil, err
			}
			requirements = append(requirements, endpoints...)
		}
	}
	if !flow.excludeRoot {
		requirements = append(requirements, readinessRequirement{service: origin, predicate: PredicateLifecycle})
	}
	return requirements, nil
}

// dependencyConsumers indexes, per dependency, the declarations of every
// service in the run that consumes it — the target included, since it is what
// the run is for even when it is itself excluded. Those declarations select
// which endpoints must be healthy: an endpoint nobody consumes cannot stand in
// for one that is required.
func (flow *Flow) dependencyConsumers(order []architecture.Service) (map[string][]*resources.ServiceDependency, error) {
	services := []*resources.Service{flow.originService}
	for _, service := range order {
		svc, err := flow.world.Dependencies.ServiceFromUnique(service.Unique)
		if err != nil {
			return nil, err
		}
		services = append(services, svc)
	}
	consumers := make(map[string][]*resources.ServiceDependency)
	for _, service := range services {
		for _, dependency := range service.ServiceDependencies {
			consumers[dependency.Unique()] = append(consumers[dependency.Unique()], dependency)
		}
	}
	return consumers, nil
}

func (flow *Flow) endpointRequirements(service string, consumers []*resources.ServiceDependency) ([]readinessRequirement, error) {
	if flow.SharedState == nil {
		return nil, nil
	}
	mappings, _ := flow.SharedState.GetNetworkMappingsFromUnique(service)
	svc, err := flow.world.Dependencies.ServiceFromUnique(service)
	if err != nil {
		return nil, err
	}
	if len(svc.Endpoints) > 0 && len(mappings) == 0 {
		return []readinessRequirement{{
			service:   service,
			predicate: PredicateMapping,
			reason:    "no network mapping recorded",
		}}, nil
	}
	endpoints := make([]*basev0.Endpoint, 0, len(mappings))
	for _, mapping := range mappings {
		if mapping.GetEndpoint() != nil {
			endpoints = append(endpoints, mapping.GetEndpoint())
		}
	}
	for _, consumer := range consumers {
		// A declared endpoint with nothing recorded behind it is a missing
		// requirement, not an absent one.
		if _, err := resources.ResolveServiceDependencyEndpoints(consumer, endpoints); err != nil {
			return []readinessRequirement{{
				service:   service,
				predicate: PredicateMapping,
				reason:    err.Error(),
			}}, nil
		}
	}
	var requirements []readinessRequirement
	for _, mapping := range mappings {
		if !endpointConsumed(consumers, mapping.GetEndpoint()) {
			continue
		}
		requirements = append(requirements, endpointRequirement(service, mapping))
	}
	return requirements, nil
}

// endpointConsumed reports whether any consumer in the run declares this
// endpoint. With no declaration in the run at all (a module-level dependency),
// every recorded endpoint counts.
func endpointConsumed(consumers []*resources.ServiceDependency, endpoint *basev0.Endpoint) bool {
	if len(consumers) == 0 {
		return true
	}
	for _, consumer := range consumers {
		if consumer.ConsumesEndpoint(endpoint.GetName(), endpoint.GetApi()) {
			return true
		}
	}
	return false
}

// endpointRequirement turns a recorded mapping into the predicate its endpoint
// declares: gRPC health for a gRPC API, a successful status for an HTTP API,
// transport reachability for everything else.
func endpointRequirement(service string, mapping *basev0.NetworkMapping) readinessRequirement {
	requirement := readinessRequirement{service: service, endpoint: mapping.GetEndpoint().GetName()}
	instance := reachableNetworkInstance(mapping.GetInstances())
	if instance == nil {
		requirement.predicate = PredicateMapping
		requirement.reason = "no network instance recorded"
		return requirement
	}
	requirement.address = networkInstanceDialAddress(instance)
	if requirement.address == "" {
		requirement.predicate = PredicateMapping
		requirement.reason = "no address recorded"
		return requirement
	}
	switch mapping.GetEndpoint().GetApi() {
	case standards.GRPC:
		requirement.predicate = PredicateGRPCHealth
		requirement.secure = strings.HasPrefix(instance.GetAddress(), "https://")
	case standards.HTTP:
		requirement.predicate = PredicateHTTPStatus
		requirement.url = endpointProbeURL(instance, requirement.address)
	default:
		requirement.predicate = PredicateTransport
	}
	return requirement
}

func endpointProbeURL(instance *basev0.NetworkInstance, address string) string {
	if strings.HasPrefix(instance.GetAddress(), "http://") || strings.HasPrefix(instance.GetAddress(), "https://") {
		return instance.GetAddress()
	}
	return "http://" + address
}

// evaluateReadinessProbes runs every network predicate with a bounded number of
// concurrent probes and reports the first requirement, in order, that fails.
func evaluateReadinessProbes(ctx context.Context, requirements []readinessRequirement) *ReadinessFailure {
	if len(requirements) == 0 {
		return nil
	}
	failures := make([]*ReadinessFailure, len(requirements))
	var group errgroup.Group
	group.SetLimit(readinessProbeConcurrency)
	for i, requirement := range requirements {
		group.Go(func() error {
			if err := requirement.probe(ctx); err != nil {
				failures[i] = requirement.failure(err.Error())
			}
			return nil
		})
	}
	_ = group.Wait()
	for _, failure := range failures {
		if failure != nil {
			return failure
		}
	}
	return nil
}

func (requirement *readinessRequirement) probe(ctx context.Context) error {
	switch requirement.predicate {
	case PredicateMapping:
		return fmt.Errorf("%s", requirement.reason)
	case PredicateGRPCHealth:
		probeCtx, cancel := context.WithTimeout(ctx, readinessProbeTimeout)
		defer cancel()
		return probeGRPCHealth(probeCtx, requirement.address, requirement.secure)
	case PredicateHTTPStatus:
		probeCtx, cancel := context.WithTimeout(ctx, readinessProbeTimeout)
		defer cancel()
		return probeHTTPStatus(probeCtx, requirement.url)
	default:
		probeCtx, cancel := context.WithTimeout(ctx, readinessTransportTimeout)
		defer cancel()
		return probeTransport(probeCtx, requirement.address)
	}
}

func probeTransport(ctx context.Context, address string) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return conn.Close()
}

func probeHTTPStatus(ctx context.Context, target string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	response, err := readinessHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("unsuccessful status %s", response.Status)
	}
	return nil
}

func probeGRPCHealth(ctx context.Context, address string, secure bool) error {
	transport := insecure.NewCredentials()
	if secure {
		transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(transport))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	response, err := healthv1.NewHealthClient(conn).Check(ctx, &healthv1.HealthCheckRequest{})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			// The server answered: it simply registers no health service, which
			// is the legacy transport-only capability. Requiring Health here
			// would break every customized server that never advertised it.
			wool.Get(ctx).Debug("gRPC endpoint is ready on transport only: no health service registered",
				wool.Field("address", address))
			return nil
		}
		return err
	}
	if response.GetStatus() != healthv1.HealthCheckResponse_SERVING {
		return fmt.Errorf("health status %s", response.GetStatus())
	}
	return nil
}

func reachableNetworkInstance(instances []*basev0.NetworkInstance) *basev0.NetworkInstance {
	for _, instance := range instances {
		if instance.GetAccess().GetKind() == resources.NetworkAccessNative {
			return instance
		}
	}
	for _, instance := range instances {
		if instance.GetAccess().GetKind() == resources.NetworkAccessPublic {
			return instance
		}
	}
	if len(instances) == 0 {
		return nil
	}
	return instances[0]
}

func networkInstanceDialAddress(instance *basev0.NetworkInstance) string {
	if instance.GetHost() != "" {
		return instance.GetHost()
	}
	if instance.GetHostname() != "" && instance.GetPort() != 0 {
		return net.JoinHostPort(instance.GetHostname(), strconv.Itoa(int(instance.GetPort())))
	}
	address := instance.GetAddress()
	u, err := url.Parse(address)
	if err == nil && u.Host != "" {
		return u.Host
	}
	return address
}
