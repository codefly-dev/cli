package orchestration

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

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
			testMapping("data", "store", "grpc", standards.GRPC, storeAddress),
			testMapping("data", "store", "admin", standards.TCP, admin.Addr().String()),
		},
		"web/console": {
			testHTTPMapping("web", "console", "http", console.URL),
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

func testMapping(module, service, endpoint, api, address string) *basev0.NetworkMapping {
	return &basev0.NetworkMapping{
		Endpoint:  &basev0.Endpoint{Name: endpoint, Module: module, Service: service, Api: api},
		Instances: []*basev0.NetworkInstance{nativeTestInstance(address)},
	}
}

func testHTTPMapping(module, service, endpoint, target string) *basev0.NetworkMapping {
	instance := nativeTestInstance("")
	instance.Address = target
	return &basev0.NetworkMapping{
		Endpoint:  &basev0.Endpoint{Name: endpoint, Module: module, Service: service, Api: standards.HTTP},
		Instances: []*basev0.NetworkInstance{instance},
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
	stack.mappings["data/store"][0] = testMapping("data", "store", "grpc", standards.GRPC, serveGRPC(t, legacy))
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
		endpointRequirement("graph/neo", testMapping("graph", "neo", "bolt", standards.TCP, bolt.Addr().String())),
		endpointRequirement("graph/neo", testHTTPMapping("graph", "neo", "http", server.URL)),
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
