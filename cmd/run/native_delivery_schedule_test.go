package run

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

var testDeliverySchedule = deliverySchedule{Deadline: 2 * time.Second, FirstBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}

func testDelivery(ready, deliver func(context.Context) error) hostDelivery {
	return hostDelivery{schedule: testDeliverySchedule, ready: ready, deliver: deliver, log: wool.Get(context.Background())}
}

func hostAlwaysReady(context.Context) error { return nil }

// answering is a host answering status on every request.
func answering(t *testing.T, status int, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/auth/authenticate" && status != http.StatusNotFound {
			_, _ = response.Write([]byte(`{"accessToken":"admin"}`))
			return
		}
		response.WriteHeader(status)
		_, _ = response.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func envelopeWriter(gatewayURL string) *gitops.EnvelopeWriter {
	return &gitops.EnvelopeWriter{GatewayURL: gatewayURL, FixtureProvider: "fixture", FixtureToken: "admin"}
}

// readEnvelope is the first host call a delivery makes, wrapped as deliver
// wraps it.
func readEnvelope(gatewayURL string) func(context.Context) error {
	return func(ctx context.Context) error {
		if _, _, err := envelopeWriter(gatewayURL).Current(ctx); err != nil {
			return fmt.Errorf("plan the module envelope: %w", err)
		}
		return nil
	}
}

// postManifest posts a module manifest straight to the host's delivery
// endpoint, as a delivery does after the envelope.
func postManifest(t *testing.T, deliveryURL string) func(context.Context) error {
	return func(ctx context.Context) error {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		signer, err := solutionhost.NewInMemorySigner("dev-test", private)
		require.NoError(t, err)
		deliverer := &gitops.NativeDeliverer{Signer: signer, DeliveryURL: deliveryURL, StateDir: t.TempDir(), Token: func(string) (string, error) { return "token", nil }}
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
	refusedURL := "http://" + closed.Addr().String()
	require.NoError(t, closed.Close())

	for _, class := range []struct {
		name    string
		fail    func(context.Context) error
		says    string // the refusal really is the class it names
		retried bool
	}{
		{"a 503 from the gateway's envelope read", readEnvelope(answering(t, http.StatusServiceUnavailable, "JWT validation not configured")), "HTTP 503", true},
		{"a 404 from the gateway before it exposes the route", readEnvelope(answering(t, http.StatusNotFound, "endpoint not exposed")), "404 Not Found", true},
		{"a 409 on the envelope write", func(ctx context.Context) error {
			_, err := envelopeWriter(answering(t, http.StatusConflict, "revision moved")).Ensure(ctx, func(uint64) ([]byte, error) { return []byte(`{}`), nil })
			return err
		}, "HTTP 409", true},
		{"no answer: connection refused", readEnvelope(refusedURL), "connection refused", true},
		{"a 500 from the host's delivery endpoint", postManifest(t, answering(t, http.StatusInternalServerError, `{"error":"boom"}`)), "(500 Internal Server Error): boom", true},
		{"a 404 from the host's delivery endpoint: a wrong endpoint", postManifest(t, answering(t, http.StatusNotFound, `{"error":"no such inbox"}`)), "(404 Not Found): no such inbox", false},
		{"a 409 from the host's delivery endpoint: a different document", postManifest(t, answering(t, http.StatusConflict, `{"error":"different document"}`)), "(409 Conflict): different document", false},
		{"a 403 from the gateway", readEnvelope(answering(t, http.StatusForbidden, "not a platform administrator")), "HTTP 403", false},
		{"a gateway address that is not a URL", readEnvelope("http://[::1"), "missing ']'", false},
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

// The shape of the platform-core run end to end through the real envelope
// client: two 503s, then the answer that lets the delivery proceed.
func TestANativeDeliveryRefusedWith503IsRetriedUntilAccepted(t *testing.T) {
	var reads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/auth/authenticate" {
			_, _ = response.Write([]byte(`{"accessToken":"admin"}`))
			return
		}
		if reads.Add(1) <= 2 {
			response.WriteHeader(http.StatusServiceUnavailable)
			_, _ = response.Write([]byte("JWT validation not configured"))
			return
		}
		response.WriteHeader(http.StatusNotFound) // no envelope written yet
	}))
	t.Cleanup(server.Close)
	require.NoError(t, testDelivery(hostAlwaysReady, readEnvelope(server.URL)).run(context.Background()))
	require.Equal(t, int64(3), reads.Load())
}

// At the deadline the error carries why the host is still not serving and the
// last refusal it answered, status and body.
func TestANativeDeliveryAtTheDeadlineNamesTheReasonAndTheLastRefusal(t *testing.T) {
	refusing := readEnvelope(answering(t, http.StatusServiceUnavailable, "JWT validation not configured"))
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
	require.ErrorContains(t, err, "HTTP 503: JWT validation not configured")
}

// fakeLauncherFlow answers readiness per service from ready, and counts the
// deliveries that reached the host's address — the first flow read of a
// delivery attempt, after its preconditions.
type fakeLauncherFlow struct {
	ready func(service string) bool

	mu                       sync.Mutex
	attempts                 int
	attemptedBeforeHostReady bool
}

func (f *fakeLauncherFlow) World() *orchestration.World { return nil }
func (f *fakeLauncherFlow) ServiceFromUnique(string) (*resources.Service, error) {
	return nil, errors.New("no service in this test")
}
func (f *fakeLauncherFlow) GetAddressForEndpoint(context.Context, string, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	f.attemptedBeforeHostReady = f.attemptedBeforeHostReady || !f.ready("saas/auth-gateway") || !f.ready("saas/accounts")
	return "", errors.New("no address in this test") // refused at once: the delivery ends after this one attempt
}
func (f *fakeLauncherFlow) EndpointCarriers(context.Context, string, *basev0.NetworkAccess) (map[string]string, error) {
	return nil, errors.New("no endpoints in this test")
}
func (f *fakeLauncherFlow) ReadyEndpointCarriers(context.Context, string, *basev0.NetworkAccess, []string) (map[string]string, error) {
	return nil, errors.New("no endpoints in this test")
}
func (f *fakeLauncherFlow) LocalRuntimeServices() []string { return []string{"model/api"} }
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

// These helpers drive a completed attempt through runReadyDelivery without
// starting an agent. Production completion is wired at Runner.Start.
func (cluster *launcher) registerRunningHook(ctx context.Context, register func(func(string, int))) {
	register(func(service string, _ int) {
		if deliver := cluster.runningDelivery(service); deliver != nil {
			go deliver(ctx)
		}
	})
}

func (cluster *launcher) onRunning(ctx context.Context, unique string) {
	if deliver := cluster.runningDelivery(unique); deliver != nil {
		deliver(ctx)
	}
}

// Capture readiness synchronously with the lifecycle notification, before the
// delivery goroutine. A restart can then invalidate it without an old queued
// callback marking the replacement process ready.
func (cluster *launcher) runningDelivery(unique string) func(context.Context) {
	identity, ok := cluster.identity(unique)
	if !ok || !cluster.delivers(identity) {
		return nil
	}
	cluster.mu.Lock()
	cluster.running[unique] = true
	waiter := cluster.waitFor(unique)
	cluster.mu.Unlock()
	return func(ctx context.Context) { cluster.runReadyDelivery(ctx, unique, identity, waiter, false) }
}
