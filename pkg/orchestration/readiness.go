package orchestration

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
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
	// PredicateHTTPStatus requires the endpoint's own address to answer without
	// a server-side failure. No health route and no health contract is
	// invented: an endpoint's root is not a health surface, so a 401, 403 or
	// 404 is proof the server is up and routing, while a 5xx (or nothing at
	// all) is proof it cannot serve.
	PredicateHTTPStatus ReadinessPredicate = "http-status"
)

const (
	// Probes are polled (every 150ms by the run loop), so they are budgeted to
	// answer rather than to wait out a slow start: a service that has not
	// answered yet is simply not ready yet, and the next poll asks again.
	readinessTransportTimeout = 500 * time.Millisecond
	readinessProbeTimeout     = time.Second
	readinessProbeConcurrency = 8
	readinessHTTPDrainLimit   = 4096
)

// readinessHTTPClient judges the endpoint that was probed, not wherever it
// points: following a redirect would make readiness depend on a third-party
// host, and off-host requests are not this probe's business.
var readinessHTTPClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

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
	// secure is the endpoint's own `secured` declaration, not a guess from its
	// address: core only ever writes a scheme into the address of HTTP-based
	// APIs, so sniffing one would leave every secured gRPC endpoint probed in
	// plaintext and permanently unready.
	secure bool
	// grpcServices are the protobuf services the endpoint declares. Health is
	// asked about each of them; with none declared the server-wide status is
	// the only thing to ask for.
	grpcServices []string
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
		return &ReadinessFailure{Predicate: PredicateRequirements, Reason: "flow has not started"}
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
	for i := range requirements {
		requirement := &requirements[i]
		if requirement.predicate != PredicateLifecycle {
			probes = append(probes, *requirement)
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

// serviceEndpointRequirements plans the endpoint requirements of one service
// with the same consumer selection readiness uses, so the live status and the
// readiness gate can never disagree about which endpoints matter.
func (flow *Flow) serviceEndpointRequirements(ctx context.Context, service string) ([]readinessRequirement, error) {
	if flow.world == nil || flow.world.Dependencies == nil {
		return nil, nil
	}
	order, err := flow.world.Dependencies.OrderTo(ctx, resources.WithUnique(flow.originService).Unique())
	if err != nil {
		return nil, err
	}
	consumers, err := flow.dependencyConsumers(order)
	if err != nil {
		return nil, err
	}
	return flow.endpointRequirements(service, consumers[service])
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
	var requirements []readinessRequirement
	for _, consumer := range consumers {
		// A declared capability with nothing recorded behind it is a missing
		// requirement, not an absent one. Declarations are validated against the
		// producer's manifest when the run set is built, so a reference that
		// resolves to no mapping here means the endpoint is not up yet.
		for _, reference := range consumer.Endpoints {
			if endpointReferenceMapping(mappings, reference) == nil {
				requirements = append(requirements, readinessRequirement{
					service:   service,
					endpoint:  reference.Name,
					predicate: PredicateMapping,
					reason:    fmt.Sprintf("declared %s has no network mapping", endpointReferenceLabel(reference)),
				})
			}
		}
	}
	for _, mapping := range mappings {
		if !endpointConsumed(consumers, mapping.GetEndpoint()) {
			continue
		}
		requirements = append(requirements, endpointRequirement(service, mapping))
	}
	return requirements, nil
}

// endpointReferenceMapping returns the recorded mapping a consumer's endpoint
// reference selects, or nil when nothing recorded matches it yet.
func endpointReferenceMapping(mappings []*basev0.NetworkMapping, reference *resources.EndpointReference) *basev0.NetworkMapping {
	for _, mapping := range mappings {
		if mapping.GetEndpoint() != nil && endpointMatchesReference(mapping.GetEndpoint(), reference) {
			return mapping
		}
	}
	return nil
}

// endpointMatchesReference reports whether an endpoint satisfies one declared
// reference. Name and API are the only keys — an endpoint's owning module and
// service cross an agent's gRPC boundary and are not required to survive it.
func endpointMatchesReference(endpoint *basev0.Endpoint, reference *resources.EndpointReference) bool {
	if reference.Name != "" && reference.Name != endpoint.GetName() {
		return false
	}
	return reference.API == "" || reference.API == endpoint.GetApi()
}

func endpointReferenceLabel(reference *resources.EndpointReference) string {
	if reference.Name != "" {
		return fmt.Sprintf("endpoint %s", reference.Name)
	}
	return fmt.Sprintf("%s API", reference.API)
}

// endpointConsumed reports whether any consumer in the run declares this
// endpoint. With no declaration in the run at all (a module-level dependency),
// every recorded endpoint counts. Selection goes through the same helper that
// decides which mappings the consumer is actually handed, so readiness can
// never require an endpoint the consumer was never given.
func endpointConsumed(consumers []*resources.ServiceDependency, endpoint *basev0.Endpoint) bool {
	if len(consumers) == 0 {
		return true
	}
	for _, consumer := range consumers {
		if dependencyConsumesEndpoint(consumer, endpoint) {
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
	endpoint := mapping.GetEndpoint()
	switch endpoint.GetApi() {
	case standards.GRPC:
		grpc := resources.IsGRPC(context.Background(), endpoint)
		requirement.predicate = PredicateGRPCHealth
		requirement.secure = grpc.GetSecured()
		requirement.grpcServices = declaredGRPCServices(grpc)
	case standards.HTTP:
		requirement.predicate = PredicateHTTPStatus
		requirement.url = endpointProbeURL(endpoint, instance, requirement.address)
	default:
		requirement.predicate = PredicateTransport
	}
	return requirement
}

// declaredGRPCServices returns the fully qualified protobuf service names the
// endpoint declares, so health is asked about the services actually consumed
// rather than only about the server-wide status a server may report SERVING
// while the consumed service is not.
func declaredGRPCServices(api *basev0.GrpcAPI) []string {
	var services []string
	for _, rpc := range api.GetRpcs() {
		name := rpc.GetServiceName()
		if name == "" {
			continue
		}
		if pkg := api.GetPackage(); pkg != "" && !strings.Contains(name, ".") {
			name = pkg + "." + name
		}
		if !slices.Contains(services, name) {
			services = append(services, name)
		}
	}
	return services
}

func endpointProbeURL(endpoint *basev0.Endpoint, instance *basev0.NetworkInstance, address string) string {
	if strings.HasPrefix(instance.GetAddress(), "http://") || strings.HasPrefix(instance.GetAddress(), "https://") {
		return instance.GetAddress()
	}
	if resources.IsHTTP(context.Background(), endpoint).GetSecured() {
		return "https://" + address
	}
	return "http://" + address
}

// evaluateReadinessProbes runs every network predicate with a bounded number of
// concurrent probes and reports the first requirement, in order, that fails.
// Results are collected in order and the rest are cancelled as soon as that
// first failure is known: readiness is polled every 150ms, and a probe that
// answers only by timing out (a Docker userland proxy accepting for a container
// that is not listening yet, say) would otherwise make every poll pay for every
// endpoint behind it.
func evaluateReadinessProbes(ctx context.Context, requirements []readinessRequirement) *ReadinessFailure {
	if len(requirements) == 0 {
		return nil
	}
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	failures := make([]*ReadinessFailure, len(requirements))
	done := make([]chan struct{}, len(requirements))
	for i := range requirements {
		done[i] = make(chan struct{})
	}
	var group errgroup.Group
	group.SetLimit(readinessProbeConcurrency)
	// Launch from its own goroutine: SetLimit makes Go block once the bound is
	// reached, so launching inline would hold the first result hostage to
	// probes ordered behind it.
	launched := make(chan struct{})
	go func() {
		defer close(launched)
		for i := range requirements {
			if probeCtx.Err() != nil {
				return
			}
			requirement := &requirements[i]
			group.Go(func() error {
				defer close(done[i])
				if err := requirement.probe(probeCtx); err != nil {
					failures[i] = requirement.failure(err.Error())
				}
				return nil
			})
		}
	}()
	defer func() {
		cancel()
		<-launched
		_ = group.Wait()
	}()
	for i := range requirements {
		select {
		case <-done[i]:
		case <-ctx.Done():
			return requirements[i].failure(ctx.Err().Error())
		}
		if failures[i] != nil {
			return failures[i]
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
		return probeGRPCHealth(probeCtx, requirement.address, requirement.secure, requirement.grpcServices)
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
	// Drain before closing so the connection returns to the pool: this probe
	// runs on every poll, and an undrained body costs a fresh socket each time.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, readinessHTTPDrainLimit))
		_ = response.Body.Close()
	}()
	if response.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("server failure status %s", response.Status)
	}
	return nil
}

func probeGRPCHealth(ctx context.Context, address string, secure bool, services []string) error {
	transport := insecure.NewCredentials()
	if secure {
		transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(transport))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	client := healthv1.NewHealthClient(conn)
	// With no declared service, the server-wide status ("") is all there is to
	// ask about.
	if len(services) == 0 {
		services = []string{""}
	}
	for _, service := range services {
		if err := checkGRPCServingStatus(ctx, client, address, service); err != nil {
			return err
		}
	}
	return nil
}

func checkGRPCServingStatus(ctx context.Context, client healthv1.HealthClient, address string, service string) error {
	response, err := client.Check(ctx, &healthv1.HealthCheckRequest{Service: service})
	if err != nil {
		switch status.Code(err) {
		case codes.Unimplemented, codes.NotFound:
			// The server answered: it registers no health service at all
			// (Unimplemented), or none for this name (NotFound, what
			// grpc-go's health server returns for a status it was never
			// given). Either way it publishes no readiness beyond the
			// transport it just proved, which is the legacy capability —
			// requiring Health here would strand every customized server
			// that never advertised it.
			wool.Get(ctx).Debug("gRPC endpoint is ready on transport only: no health status published",
				wool.Field("address", address), wool.Field("service", service))
			return nil
		default:
			return err
		}
	}
	if response.GetStatus() != healthv1.HealthCheckResponse_SERVING {
		if service == "" {
			return fmt.Errorf("health status %s", response.GetStatus())
		}
		return fmt.Errorf("health status %s for %s", response.GetStatus(), service)
	}
	return nil
}
