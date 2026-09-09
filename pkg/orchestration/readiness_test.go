package orchestration

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codefly-dev/core/architecture"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/tui"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
)

// readinessStack is the acceptance shape of the audit: a target consuming one
// gRPC endpoint of a dependency that also exposes an unrelated admin port, an
// endpointless job, an HTTP dependency, and one endpointless service the target
// requires only transitively. Every dependency is a real server on a real
// socket.
type readinessStack struct {
	flow          *Flow
	health        *health.Server
	store         *grpc.Server
	admin         net.Listener
	console       *httptest.Server
	consoleStatus *atomic.Int64
	mappings      map[string][]*basev0.NetworkMapping
}

func newReadinessStack(t *testing.T) *readinessStack {
	t.Helper()
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/readiness")
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "app")
	require.NoError(t, err)
	origin, err := module.LoadServiceFromName(ctx, "api")
	require.NoError(t, err)
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)

	healthServer := health.NewServer()
	store := grpc.NewServer()
	healthv1.RegisterHealthServer(store, healthServer)
	storeAddress := serveGRPC(t, store)

	admin, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	consoleStatus := &atomic.Int64{}
	consoleStatus.Store(http.StatusOK)
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(consoleStatus.Load()))
	}))
	t.Cleanup(console.Close)

	mappings := map[string][]*basev0.NetworkMapping{
		"data/store": {
			testMapping("grpc", standards.GRPC, storeAddress),
			testMapping("admin", standards.TCP, admin.Addr().String()),
		},
		"web/console": {
			testHTTPMapping("http", console.URL),
		},
	}

	flow := &Flow{
		originService: origin,
		world:         &World{Dependencies: dependencies},
		playbook:      &Playbook{},
		excludeRoot:   true,
		SharedState:   &StateManager{networkMappings: mappings},
	}
	for _, dependency := range []string{"data/store", "data/seed", "jobs/migrate", "web/console"} {
		flow.emitState(dependency, tui.StateRunning, 0)
	}
	return &readinessStack{
		flow:          flow,
		health:        healthServer,
		store:         store,
		admin:         admin,
		console:       console,
		consoleStatus: consoleStatus,
		mappings:      mappings,
	}
}

func serveGRPC(t *testing.T, server *grpc.Server) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

// testMapping deliberately leaves the endpoint's owning module and service
// empty. Recorded mappings are built from what an agent returns over gRPC, and
// nothing in the run path guarantees those fields survive that boundary — so
// readiness must select endpoints without them.
func testMapping(endpoint, api, address string) *basev0.NetworkMapping {
	return &basev0.NetworkMapping{
		Endpoint:  &basev0.Endpoint{Name: endpoint, Api: api},
		Instances: []*basev0.NetworkInstance{nativeTestInstance(address)},
	}
}

func testHTTPMapping(endpoint, target string) *basev0.NetworkMapping {
	instance := nativeTestInstance("")
	instance.Address = target
	return &basev0.NetworkMapping{
		Endpoint:  &basev0.Endpoint{Name: endpoint, Api: standards.HTTP},
		Instances: []*basev0.NetworkInstance{instance},
	}
}

func nativeTestInstance(host string) *basev0.NetworkInstance {
	return &basev0.NetworkInstance{
		Access: &basev0.NetworkAccess{Kind: resources.NetworkAccessNative},
		Host:   host,
	}
}

func TestReadinessHoldsWhenEveryRequirementIsSatisfied(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()
	require.Nil(t, stack.flow.Readiness(ctx))
	require.True(t, stack.flow.Ready(ctx))
}

func TestReadinessRequiresConsumedGRPCEndpointDespiteOpenAdminPort(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()
	require.True(t, stack.flow.Ready(ctx))

	// The admin port stays open; only the consumed gRPC endpoint goes away.
	stack.store.Stop()

	failure := stack.flow.Readiness(ctx)
	require.NotNil(t, failure)
	require.Equal(t, "data/store", failure.Service)
	require.Equal(t, "grpc", failure.Endpoint)
	require.Equal(t, PredicateGRPCHealth, failure.Predicate)
	require.NotEmpty(t, failure.Reason)
	require.False(t, stack.flow.Ready(ctx))
}

func TestReadinessRejectsNotServingGRPCHealth(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()

	stack.health.SetServingStatus("", healthv1.HealthCheckResponse_NOT_SERVING)
	failure := stack.flow.Readiness(ctx)
	require.NotNil(t, failure)
	require.Equal(t, "data/store", failure.Service)
	require.Equal(t, PredicateGRPCHealth, failure.Predicate)
	require.Contains(t, failure.Reason, "NOT_SERVING")

	stack.health.SetServingStatus("", healthv1.HealthCheckResponse_SERVING)
	require.True(t, stack.flow.Ready(ctx))
}

func TestReadinessAcceptsGRPCServerWithoutHealthServiceAsTransportOnly(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()

	// A customized server that never registered gRPC Health answers
	// Unimplemented: it declares transport-only readiness, and inventing a
	// Health requirement for it would break it.
	legacy := grpc.NewServer()
	stack.mappings["data/store"][0] = testMapping("grpc", standards.GRPC, serveGRPC(t, legacy))
	require.Nil(t, stack.flow.Readiness(ctx))
}

func TestReadinessRejectsUnsuccessfulHTTPStatus(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()

	stack.consoleStatus.Store(http.StatusServiceUnavailable)
	failure := stack.flow.Readiness(ctx)
	require.NotNil(t, failure)
	require.Equal(t, "web/console", failure.Service)
	require.Equal(t, "http", failure.Endpoint)
	require.Equal(t, PredicateHTTPStatus, failure.Predicate)
	require.Contains(t, failure.Reason, "503")

	stack.consoleStatus.Store(http.StatusOK)
	require.True(t, stack.flow.Ready(ctx))
}

func TestReadinessRequiresLifecycleCompletionOfEndpointlessDependency(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()

	// The job is still running: having no endpoint is not evidence of readiness.
	stack.flow.emitState("jobs/migrate", tui.StateStarting, 0)
	failure := stack.flow.Readiness(ctx)
	require.NotNil(t, failure)
	require.Equal(t, "jobs/migrate", failure.Service)
	require.Equal(t, PredicateLifecycle, failure.Predicate)

	stack.flow.emitState("jobs/migrate", tui.StateRunning, 0)
	require.True(t, stack.flow.Ready(ctx))
}

func TestReadinessRequiresTransitiveDependency(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()

	// data/seed is required by data/store, not by the target: a direct socket
	// list would never look at it.
	stack.flow.emitState("data/seed", tui.StateStarting, 0)
	failure := stack.flow.Readiness(ctx)
	require.NotNil(t, failure)
	require.Equal(t, "data/seed", failure.Service)
	require.Equal(t, PredicateLifecycle, failure.Predicate)

	stack.flow.emitState("data/seed", tui.StateRunning, 0)
	require.True(t, stack.flow.Ready(ctx))
}

func TestReadinessRequiresLifecycleBeforeProbingSockets(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()

	// Every socket is open, but the run has not finished starting the service:
	// a listener on its deterministically hashed port is not our service.
	stack.flow.emitState("data/store", tui.StateStarting, 0)
	failure := stack.flow.Readiness(ctx)
	require.NotNil(t, failure)
	require.Equal(t, "data/store", failure.Service)
	require.Equal(t, PredicateLifecycle, failure.Predicate)
	require.Empty(t, failure.Endpoint)
}

func TestReadinessRevokedByRunnerFailureAfterStart(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()
	require.True(t, stack.flow.Ready(ctx))

	stack.flow.reportFailure("data/store", "process exited")

	failure := stack.flow.Readiness(ctx)
	require.NotNil(t, failure)
	require.Equal(t, "data/store", failure.Service)
	require.Equal(t, PredicateRunner, failure.Predicate)
	require.Contains(t, failure.Reason, "process exited")
}

func TestReadinessIgnoresEndpointNoConsumerDeclares(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()

	// api consumes store's grpc endpoint only: admin going down says nothing
	// about what the run requires.
	require.NoError(t, stack.admin.Close())
	require.Nil(t, stack.flow.Readiness(ctx))
}

func TestReadinessRequiresMappingForDeclaredEndpoint(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()

	stack.mappings["data/store"] = stack.mappings["data/store"][1:]
	failure := stack.flow.Readiness(ctx)
	require.NotNil(t, failure)
	require.Equal(t, "data/store", failure.Service)
	require.Equal(t, PredicateMapping, failure.Predicate)
}

func TestReadinessExcludedRootDoesNotRequireOriginStart(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()
	require.True(t, stack.flow.Ready(ctx))

	stack.flow.excludeRoot = false
	failure := stack.flow.Readiness(ctx)
	require.NotNil(t, failure)
	require.Equal(t, "app/api", failure.Service)
	require.Equal(t, PredicateLifecycle, failure.Predicate)

	stack.flow.emitState("app/api", tui.StateRunning, 0)
	require.True(t, stack.flow.Ready(ctx))
}

func TestReadinessResolvesNamedEndpointWithoutOwningIdentity(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()

	// app/api declares store's "grpc" endpoint by name. The recorded mapping
	// carries no owning module or service — the shape an agent may hand back —
	// and selection must still resolve it in both directions.
	require.Nil(t, stack.flow.Readiness(ctx))

	stack.mappings["data/store"] = stack.mappings["data/store"][1:]
	failure := stack.flow.Readiness(ctx)
	require.NotNil(t, failure)
	require.Equal(t, "data/store", failure.Service)
	require.Equal(t, PredicateMapping, failure.Predicate)
	require.Contains(t, failure.Reason, "grpc")
}

func TestReadinessAcceptsHealthServerWithoutStatusForCheckedService(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()

	// The endpoint declares a service the health server publishes no status
	// for, so Check answers NotFound. The server answered: it publishes nothing
	// to gate on, which is the transport-only capability — treating NotFound as
	// a failure would strand it as permanently unready.
	stack.mappings["data/store"][0].Endpoint.ApiDetails = resources.ToGrpcAPI(&basev0.GrpcAPI{
		Package: "acme.v1",
		Rpcs:    []*basev0.RPC{{ServiceName: "Store", Name: "Get"}},
	})
	require.Nil(t, stack.flow.Readiness(ctx))

	// It is still a real probe: the endpoint going away is still not ready.
	stack.store.Stop()
	failure := stack.flow.Readiness(ctx)
	require.NotNil(t, failure)
	require.Equal(t, PredicateGRPCHealth, failure.Predicate)
}

func TestReadinessChecksDeclaredGRPCServices(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()

	mapping := stack.mappings["data/store"][0]
	mapping.Endpoint.ApiDetails = resources.ToGrpcAPI(&basev0.GrpcAPI{
		Package: "acme.v1",
		Rpcs:    []*basev0.RPC{{ServiceName: "Store", Name: "Get"}},
	})
	stack.health.SetServingStatus("acme.v1.Store", healthv1.HealthCheckResponse_NOT_SERVING)

	// The server-wide status is SERVING; only the consumed service is not.
	failure := stack.flow.Readiness(ctx)
	require.NotNil(t, failure)
	require.Equal(t, PredicateGRPCHealth, failure.Predicate)
	require.Contains(t, failure.Reason, "acme.v1.Store")

	stack.health.SetServingStatus("acme.v1.Store", healthv1.HealthCheckResponse_SERVING)
	require.True(t, stack.flow.Ready(ctx))
}

func TestReadinessUsesDeclaredTransportSecurity(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()
	require.True(t, stack.flow.Ready(ctx))

	// The endpoint declares TLS; the server speaks plaintext. The declaration
	// decides how the endpoint is probed — an address never carries a scheme
	// for gRPC, so sniffing one would silently probe every secured endpoint in
	// plaintext instead.
	mapping := stack.mappings["data/store"][0]
	mapping.Endpoint.ApiDetails = resources.ToGrpcAPI(&basev0.GrpcAPI{Secured: true})
	requirement := endpointRequirement("data/store", mapping)
	require.True(t, requirement.secure)

	failure := stack.flow.Readiness(ctx)
	require.NotNil(t, failure)
	require.Equal(t, PredicateGRPCHealth, failure.Predicate)
}

func TestReadinessAcceptsClientErrorsAndRejectsServerFailures(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()

	// An endpoint's root is not a health surface: an authenticated or routed
	// service answering 401/404 has proved it is up and routing.
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		stack.consoleStatus.Store(int64(code))
		require.Nil(t, stack.flow.Readiness(ctx), "status %d must not block readiness", code)
	}

	for _, code := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable} {
		stack.consoleStatus.Store(int64(code))
		failure := stack.flow.Readiness(ctx)
		require.NotNil(t, failure, "status %d must block readiness", code)
		require.Equal(t, PredicateHTTPStatus, failure.Predicate)
	}
}

func TestReadinessDoesNotFollowRedirects(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A redirect to a port nothing listens on: readiness judges the
		// endpoint that was probed, never where it points.
		http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
	}))
	defer server.Close()

	requirements := []readinessRequirement{endpointRequirement("web/console", testHTTPMapping("http", server.URL))}
	require.Nil(t, evaluateReadinessProbes(ctx, requirements))
}

func TestServiceReachableIgnoresEndpointNoConsumerDeclares(t *testing.T) {
	stack := newReadinessStack(t)
	ctx := context.Background()
	require.True(t, stack.flow.ServiceReachable(ctx, "data/store"))

	// api consumes store's grpc endpoint only. The live status must not keep
	// showing the dependency as not up over an endpoint the run never needs.
	require.NoError(t, stack.admin.Close())
	require.True(t, stack.flow.ServiceReachable(ctx, "data/store"))

	stack.store.Stop()
	require.False(t, stack.flow.ServiceReachable(ctx, "data/store"))
}

func TestReadinessProbesReportFirstFailureWithoutWaitingOnSlowerOnes(t *testing.T) {
	ctx := context.Background()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closedAddress := closed.Addr().String()
	require.NoError(t, closed.Close())

	// A listener that accepts and never speaks gRPC: its health probe can only
	// end by timing out.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = silent.Close() }()
	go func() {
		for {
			conn, acceptErr := silent.Accept()
			if acceptErr != nil {
				return
			}
			defer func() { _ = conn.Close() }()
		}
	}()

	requirements := []readinessRequirement{
		endpointRequirement("data/store", testMapping("admin", standards.TCP, closedAddress)),
		endpointRequirement("data/store", testMapping("grpc", standards.GRPC, silent.Addr().String())),
	}
	started := time.Now()
	failure := evaluateReadinessProbes(ctx, requirements)
	elapsed := time.Since(started)
	require.NotNil(t, failure)
	require.Equal(t, "admin", failure.Endpoint)
	require.Less(t, elapsed, readinessProbeTimeout, "a decided failure must not wait on probes behind it")
}

func TestReadinessReportsUnplannedFlowAsRequirements(t *testing.T) {
	failure := (&Flow{}).Readiness(context.Background())
	require.NotNil(t, failure)
	require.Equal(t, PredicateRequirements, failure.Predicate)
}

// TestDependencyEndpointDeclarationsValidatedWhenRunSetIsBuilt covers the
// manifest error that used to be discovered only by readiness waiting for a
// mapping that could never appear — a typo that turned into a run stuck in
// "Starting" instead of an error naming the declaration.
func TestDependencyEndpointDeclarationsValidatedWhenRunSetIsBuilt(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name      string
		workspace string
		module    string
		service   string
		wantError string
	}{
		// web/console exposes two http endpoints and app/api consumes it without
		// naming any: that resolves at runtime, so it must not fail the run.
		{name: "resolvable", workspace: "testdata/readiness", module: "app", service: "api"},
		{name: "undeclared endpoint", workspace: "testdata/readiness-bad-endpoint", module: "app", service: "api", wantError: "gprc"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, err := resources.LoadWorkspaceFromDir(ctx, test.workspace)
			require.NoError(t, err)
			module, err := workspace.LoadModuleFromName(ctx, test.module)
			require.NoError(t, err)
			origin, err := module.LoadServiceFromName(ctx, test.service)
			require.NoError(t, err)
			dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
			require.NoError(t, err)
			flow := &Flow{originService: origin, world: &World{Dependencies: dependencies}}

			order, err := dependencies.OrderTo(ctx, resources.WithUnique(origin).Unique())
			require.NoError(t, err)
			var required []string
			for _, service := range order {
				required = append(required, service.Unique)
			}

			err = flow.validateDependencyEndpointDeclarations(required)
			if test.wantError == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), test.wantError)
			require.Contains(t, err.Error(), "app/api")
		})
	}
}

// TestReadinessProbesBoltAndHTTPByDeclaredAPI replaces the endpoint-name
// special case that used to give Neo4j its bolt+http treatment: the same pair
// is now evaluated from what each endpoint declares, and the HTTP half must
// answer successfully rather than merely answer.
func TestReadinessProbesBoltAndHTTPByDeclaredAPI(t *testing.T) {
	ctx := context.Background()
	bolt, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = bolt.Close() }()

	status := &atomic.Int64{}
	status.Store(http.StatusServiceUnavailable)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()

	requirements := []readinessRequirement{
		endpointRequirement("graph/neo", testMapping("bolt", standards.TCP, bolt.Addr().String())),
		endpointRequirement("graph/neo", testHTTPMapping("http", server.URL)),
	}
	require.Equal(t, PredicateTransport, requirements[0].predicate)
	require.Equal(t, PredicateHTTPStatus, requirements[1].predicate)

	failure := evaluateReadinessProbes(ctx, requirements)
	require.NotNil(t, failure)
	require.Equal(t, "http", failure.Endpoint)

	status.Store(http.StatusOK)
	require.Nil(t, evaluateReadinessProbes(ctx, requirements))

	require.NoError(t, bolt.Close())
	failure = evaluateReadinessProbes(ctx, requirements)
	require.NotNil(t, failure)
	require.Equal(t, "bolt", failure.Endpoint)
}
