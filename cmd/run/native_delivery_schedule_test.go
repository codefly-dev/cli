package run

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var testDeliverySchedule = deliverySchedule{Deadline: 2 * time.Second, FirstBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}

func testDelivery(ready, deliver func(context.Context) error) hostDelivery {
	return hostDelivery{schedule: testDeliverySchedule, ready: ready, deliver: deliver, log: wool.Get(context.Background())}
}

func hostAlwaysReady(context.Context) error { return nil }

// refusingComposition is a composition host that refuses the first refusals
// ApplyManifest calls with their codes, then accepts.
type refusingComposition struct {
	compositionv1.UnimplementedCompositionServiceServer
	mu       sync.Mutex
	refusals []codes.Code
	calls    int
}

func (h *refusingComposition) ApplyManifest(_ context.Context, req *compositionv1.ApplyManifestRequest) (*compositionv1.ApplyManifestResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if len(h.refusals) > 0 {
		code := h.refusals[0]
		h.refusals = h.refusals[1:]
		return nil, status.Errorf(code, "refused with %s", code)
	}
	manifest, err := solutionhost.ManifestFromVerified(req.GetCarrier().GetDocument())
	if err != nil {
		return nil, err
	}
	return &compositionv1.ApplyManifestResponse{Principal: manifest.Alias, Generation: manifest.Generation}, nil
}

// serveComposition serves host — nil serves no composition interface at all —
// on a loopback port, and answers how the run reaches it.
func serveComposition(t *testing.T, host compositionv1.CompositionServiceServer) orchestration.GRPCTarget {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	if host != nil {
		compositionv1.RegisterCompositionServiceServer(server, host)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return orchestration.GRPCTarget{Address: listener.Addr().String()}
}

// deliverTo delivers a module manifest through the composition interface at
// target, as a delivery attempt does.
func deliverTo(t *testing.T, target orchestration.GRPCTarget) func(context.Context) error {
	return func(ctx context.Context) error {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		signer, err := solutionhost.NewInMemorySigner("dev-test", private)
		require.NoError(t, err)
		conn, err := target.Dial()
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		composition := gitops.NewCompositionDelivery(conn, signer, nil, nil, nil)
		deliverer := &gitops.NativeDeliverer{Signer: signer, StateDir: t.TempDir(), Prepare: composition.Prepare, Token: func(string) (string, error) { return "token", nil }}
		_, err = deliverer.Deliver(ctx, &solutionhost.ModuleManifest{
			Schema: solutionhost.SchemaModuleManifestV1, Binding: "acme.local.model", Alias: "model", Kind: solutionhost.KindModule,
			Host:      solutionhost.HostTarget{Coordinate: "example/local/dev", Domain: "acme"},
			Release:   solutionhost.Release{Publisher: "acme", Name: "model", Version: "0.1.0"},
			Workloads: []solutionhost.Workload{{Service: "model", Container: "model"}},
			Authority: &solutionhost.Authority{Principal: "model"},
		})
		return err
	}
}

// Each class of refusal, produced by the real client against a host that
// answers it once: the ones that say "not serving yet" are asked again, the
// ones that say no are not.
func TestANativeDeliveryRetriesExactlyTheRefusalsThatMeanNotServingYet(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refused := orchestration.GRPCTarget{Address: closed.Addr().String()}
	require.NoError(t, closed.Close())
	refusing := func(code codes.Code) func(context.Context) error {
		return deliverTo(t, serveComposition(t, &refusingComposition{refusals: []codes.Code{code}}))
	}

	for _, class := range []struct {
		name    string
		fail    func(context.Context) error
		says    string // the refusal really is the class it names
		retried bool
	}{
		{"the host is unavailable", refusing(codes.Unavailable), "code = Unavailable", true},
		{"the host is out of time", refusing(codes.DeadlineExceeded), "code = DeadlineExceeded", true},
		{"the host is out of capacity", refusing(codes.ResourceExhausted), "code = ResourceExhausted", true},
		{"no answer: connection refused", deliverTo(t, refused), "code = Unavailable", true},
		{"the endpoint serves no composition interface", deliverTo(t, serveComposition(t, nil)), "code = Unimplemented", false},
		{"an internal failure", refusing(codes.Internal), "code = Internal", false},
		{"a manifest the host cannot read", refusing(codes.InvalidArgument), "code = InvalidArgument", false},
		{"a carrier the host denies", refusing(codes.PermissionDenied), "code = PermissionDenied", false},
		{"a stale generation", refusing(codes.FailedPrecondition), "code = FailedPrecondition", false},
		{"a manifest the host has not applied for a reason it names", func(context.Context) error {
			return errors.New("module model is delivered and not served: the host rejected generation 1: binding withdrawn")
		}, "not served", false},
	} {
		t.Run(class.name, func(t *testing.T) {
			attempts := 0
			err := testDelivery(hostAlwaysReady, func(ctx context.Context) error {
				attempts++
				if attempts == 1 {
					failure := class.fail(ctx)
					require.ErrorContains(t, failure, class.says)
					return failure
				}
				return nil
			}).run(context.Background())
			if class.retried {
				require.NoError(t, err)
				require.Equal(t, 2, attempts)
				return
			}
			require.Error(t, err)
			require.Equal(t, 1, attempts, "a refusal that says no is not asked again")
		})
	}
}

// A host that is unavailable twice, then accepts, through the real client.
func TestANativeDeliveryRefusedAsUnavailableIsRetriedUntilAccepted(t *testing.T) {
	host := &refusingComposition{refusals: []codes.Code{codes.Unavailable, codes.Unavailable}}
	require.NoError(t, testDelivery(hostAlwaysReady, deliverTo(t, serveComposition(t, host))).run(context.Background()))
	require.Equal(t, 3, host.calls)
}

// At the deadline the error carries why the host is still not serving and the
// last refusal it answered.
func TestANativeDeliveryAtTheDeadlineNamesTheReasonAndTheLastRefusal(t *testing.T) {
	refusing := deliverTo(t, serveComposition(t, &refusingComposition{refusals: []codes.Code{codes.Unavailable, codes.Unavailable, codes.Unavailable, codes.Unavailable}}))
	ready := true
	delivery := testDelivery(func(context.Context) error {
		if ready {
			ready = false
			return nil
		}
		return errors.New("the host is not ready: saas/auth-gateway: lifecycle: runtime start has not completed")
	}, refusing)
	delivery.schedule.Deadline = 30 * time.Millisecond
	err := delivery.run(context.Background())
	require.ErrorContains(t, err, "configured bound 30ms")
	require.ErrorContains(t, err, "saas/auth-gateway: lifecycle: runtime start has not completed")
	require.ErrorContains(t, err, "refused with Unavailable")
}

// fakeLauncherFlow answers readiness per service from ready, and counts the
// deliveries that reached the host's address — the first flow read of a
// delivery attempt, after its preconditions.
type fakeLauncherFlow struct {
	ready   func(service string) bool
	runtime string

	mu                       sync.Mutex
	attempts                 int
	attemptedBeforeHostReady bool
}

func (f *fakeLauncherFlow) World() *orchestration.World { return nil }
func (f *fakeLauncherFlow) ServiceFromUnique(string) (*resources.Service, error) {
	return nil, errors.New("no service in this test")
}
func (f *fakeLauncherFlow) GRPCEndpointTarget(context.Context, string, string, string) (orchestration.GRPCTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	f.attemptedBeforeHostReady = f.attemptedBeforeHostReady || !f.ready("saas/auth-gateway") || !f.ready("saas/accounts")
	return orchestration.GRPCTarget{}, errors.New("no address in this test") // refused at once: the delivery ends after this one attempt
}
func (f *fakeLauncherFlow) ServiceRuntimeContext(string) string {
	if f.runtime != "" {
		return f.runtime
	}
	return resources.RuntimeContextNative
}
func (f *fakeLauncherFlow) EndpointCarriers(context.Context, string, *basev0.NetworkAccess) (map[string]string, error) {
	return nil, errors.New("no endpoints in this test")
}
func (f *fakeLauncherFlow) ServiceReadiness(_ context.Context, service string) *orchestration.ReadinessFailure {
	if f.ready(service) {
		return nil
	}
	return &orchestration.ReadinessFailure{Service: service, Predicate: orchestration.PredicateLifecycle, Reason: "runtime start has not completed"}
}

// launcherWithFakeFlow composes module "model" for a host whose gateway is
// saas/auth-gateway and whose delivery service is saas/accounts.
func launcherWithFakeFlow(t *testing.T, ready func(string) bool) (*launcher, *fakeLauncherFlow, string) {
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), writeNonGoWorkloadDir(t, "model"))
	require.NoError(t, err)
	unique := "model/api"
	flow := &fakeLauncherFlow{ready: ready}
	return &launcher{
		workspace: workspace, flow: flow, hostModule: "saas", hostService: "accounts",
		env:        &environments.Environment{Host: &environments.EnvironmentHost{Gateway: "saas/auth-gateway", Delivery: "saas/accounts/rest"}},
		identities: map[string]gitops.NativeIdentity{unique: {Module: "model", Service: "api"}},
		running:    map[string]bool{},
		delivered:  map[string]*solutionhost.ModuleManifest{},
		schedule:   testDeliverySchedule,
	}, flow, unique
}

// A module Running before the gateway is delivered once the gateway is ready,
// once, and not before.
func TestAModuleRunningBeforeTheGatewayIsDeliveredOnceTheGatewayIsReady(t *testing.T) {
	var checks atomic.Int64
	cluster, flow, unique := launcherWithFakeFlow(t, func(service string) bool {
		if service == "saas/auth-gateway" {
			return checks.Add(1) > 3 // the gateway finishes starting
		}
		return true
	})
	cluster.onRunning(context.Background(), unique)
	require.Equal(t, 1, flow.attempts, "delivered once the gateway is ready")
	require.False(t, flow.attemptedBeforeHostReady, "never delivered to a gateway that is not ready")
}

// The delivery service is part of the host: a gateway that is ready in front
// of a delivery service that is not is not a host to deliver to.
func TestANativeDeliveryWaitsForTheHostsDeliveryServiceToo(t *testing.T) {
	cluster, flow, unique := launcherWithFakeFlow(t, func(service string) bool { return service != "saas/accounts" })
	cluster.schedule.Deadline = 30 * time.Millisecond
	cluster.onRunning(context.Background(), unique)
	require.Zero(t, flow.attempts)
	require.ErrorContains(t, cluster.hostReady(context.Background()), "saas/accounts: lifecycle: runtime start has not completed")
}

// A workload the composition withdraws while its delivery waits is not
// delivered on its behalf.
func TestANativeDeliveryOfAWithdrawnWorkloadStops(t *testing.T) {
	var cluster *launcher
	var unique string
	cluster, flow, unique := launcherWithFakeFlow(t, func(string) bool {
		cluster.forget(unique)
		return false
	})
	cluster.stateDir = t.TempDir()
	cluster.schedule.Deadline = time.Minute
	done := make(chan struct{})
	go func() {
		defer close(done)
		cluster.onRunning(context.Background(), unique)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the delivery of a withdrawn workload is still waiting")
	}
	require.Zero(t, flow.attempts)
}

// A workload that starts again replaces its waiting delivery: one waiter per
// workload, and the run stopping ends a wait without reporting anything.
func TestARestartedWorkloadSupersedesItsWaitingDelivery(t *testing.T) {
	cluster, flow, unique := launcherWithFakeFlow(t, func(string) bool { return false })
	cluster.schedule.Deadline = time.Minute
	done := make(chan struct{})
	go func() {
		defer close(done)
		cluster.onRunning(context.Background(), unique)
	}()
	require.Eventually(t, func() bool {
		cluster.waitersMu.Lock()
		defer cluster.waitersMu.Unlock()
		return cluster.waiters[unique] != nil
	}, time.Second, time.Millisecond)
	next := cluster.waitFor(unique)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the superseded delivery is still waiting")
	}
	cluster.doneWaiting(unique, next)
	require.Zero(t, flow.attempts)

	stop := make(chan struct{})
	close(stop)
	delivery := testDelivery(func(context.Context) error { return errors.New("not ready") }, hostAlwaysReady)
	delivery.stop = stop
	require.ErrorIs(t, delivery.run(context.Background()), errRunStopped)
}
