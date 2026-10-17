package run

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/nativecluster"
	"github.com/codefly-dev/cli/pkg/orchestration"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
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

// postEndpoints posts straight to the host's delivery endpoint, as a delivery
// does after the envelope.
func postEndpoints(deliveryURL string) func(context.Context) error {
	return func(ctx context.Context) error {
		deliverer := &gitops.NativeDeliverer{Signer: solutionhost.FixtureSigner(), DeliveryURL: deliveryURL, Token: func(string) (string, error) { return "token", nil }}
		presence := &solutionhost.SolutionHostBinding{Workloads: []solutionhost.Workload{{Identity: solutionhost.WorkloadIdentity{SPIFFEID: "spiffe://cluster.local/ns/acme/sa/model"}}}}
		raw, err := (&solutionhost.EndpointDelivery{
			Schema: solutionhost.EndpointDeliverySchema, Host: solutionhost.HostTarget{Coordinate: "example/local/dev", Component: "host"},
			Binding: "acme.model", Generation: 1, OwnershipDomain: "acme", PresenceDigest: "sha256:" + strings.Repeat("0", 64), Carriers: map[string]string{},
		}).CanonicalBytes()
		if err != nil {
			return err
		}
		_, err = deliverer.DeliverEndpoints(ctx, presence, raw)
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
			_, commit, err := envelopeWriter(answering(t, http.StatusConflict, "revision moved")).Plan(ctx, func(uint64, []byte) ([]byte, error) { return []byte(`{}`), nil })
			if err == nil {
				err = commit(ctx)
			}
			return err
		}, "HTTP 409", true},
		{"no answer: connection refused", readEnvelope(refusedURL), "connection refused", true},
		{"a 500 from the host's delivery endpoint", postEndpoints(answering(t, http.StatusInternalServerError, `{"error":"boom"}`)), "(500 Internal Server Error): boom", true},
		{"a 404 from the host's delivery endpoint: a wrong endpoint", postEndpoints(answering(t, http.StatusNotFound, `{"error":"no such inbox"}`)), "(404 Not Found): no such inbox", false},
		{"a 409 from the host's delivery endpoint: a different document", postEndpoints(answering(t, http.StatusConflict, `{"error":"different document"}`)), "(409 Conflict): different document", false},
		{"a 403 from the gateway", readEnvelope(answering(t, http.StatusForbidden, "not a platform administrator")), "HTTP 403", false},
		{"a gateway address that is not a URL", readEnvelope("http://[::1"), "missing ']'", false},
		{"an agent that reports no started artifact", func(context.Context) error {
			return errors.New("agent codefly.dev/go-grpc:0.1.55 reports no started artifact (runtime-started-artifact/v1)")
		}, "no started artifact", false},
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
	require.ErrorContains(t, err, "did not accept the delivery within 30ms")
	require.ErrorContains(t, err, "saas/auth-gateway: lifecycle: runtime start has not completed")
	require.ErrorContains(t, err, "HTTP 503: JWT validation not configured")
}

// fakeNativeFlow answers readiness per service from ready, and counts the
// deliveries that reached the agent's report of what it started — the first
// read of a delivery attempt, after its preconditions.
type fakeNativeFlow struct {
	service *resources.Service
	ready   func(service string) bool

	mu                       sync.Mutex
	attempts                 int
	attemptedBeforeHostReady bool
}

func (f *fakeNativeFlow) World() *orchestration.World { return nil }
func (f *fakeNativeFlow) ServiceFromUnique(string) (*resources.Service, error) {
	return f.service, nil
}
func (f *fakeNativeFlow) GetAddressForEndpoint(context.Context, string, string, string) (string, error) {
	return "", errors.New("no address in this test")
}
func (f *fakeNativeFlow) EndpointDelivery(context.Context, *solutionhost.SolutionHostBinding, *basev0.NetworkAccess) ([]byte, error) {
	return nil, errors.New("no endpoints in this test")
}
func (f *fakeNativeFlow) ServiceReadiness(_ context.Context, service string) *orchestration.ReadinessFailure {
	if f.ready(service) {
		return nil
	}
	return &orchestration.ReadinessFailure{Service: service, Predicate: orchestration.PredicateLifecycle, Reason: "runtime start has not completed"}
}
func (f *fakeNativeFlow) StartedArtifact(string) *runtimev0.StartedArtifact {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	f.attemptedBeforeHostReady = f.attemptedBeforeHostReady || !f.ready("saas/auth-gateway") || !f.ready("saas/accounts")
	return nil // refused at once: the delivery ends after this one attempt
}

// clusterWithFakeFlow composes module "model" for a host whose gateway is
// saas/auth-gateway and whose delivery service is saas/accounts.
func clusterWithFakeFlow(t *testing.T, ready func(string) bool) (*nativeCluster, *fakeNativeFlow, string) {
	workspace, service := writeNonGoWorkload(t)
	unique := resources.WithUnique(service).Unique()
	flow := &fakeNativeFlow{service: service, ready: ready}
	return &nativeCluster{
		workspace: workspace, flow: flow, hostModule: "saas", hostService: "accounts",
		env:        &environments.Environment{Host: &environments.EnvironmentHost{Gateway: "saas/auth-gateway", Delivery: "saas/accounts/rest"}},
		identities: map[string]gitops.NativeIdentity{unique: {Module: "model", Service: "model", Account: "model"}},
		workloads:  map[string]nativecluster.Workload{unique: {Namespace: "acme-model", Account: "model"}},
		schedule:   testDeliverySchedule,
	}, flow, unique
}

// A module Running before the gateway is delivered once the gateway is ready,
// once, and not before.
func TestAModuleRunningBeforeTheGatewayIsDeliveredOnceTheGatewayIsReady(t *testing.T) {
	var checks atomic.Int64
	cluster, flow, unique := clusterWithFakeFlow(t, func(service string) bool {
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
	cluster, flow, unique := clusterWithFakeFlow(t, func(service string) bool { return service != "saas/accounts" })
	cluster.schedule.Deadline = 30 * time.Millisecond
	cluster.onRunning(context.Background(), unique)
	require.Zero(t, flow.attempts)
	require.ErrorContains(t, cluster.hostReady(context.Background()), "saas/accounts: lifecycle: runtime start has not completed")
}

// A workload the composition withdraws while its delivery waits is not
// delivered on its behalf.
func TestANativeDeliveryOfAWithdrawnWorkloadStops(t *testing.T) {
	var cluster *nativeCluster
	var unique string
	cluster, flow, unique := clusterWithFakeFlow(t, func(string) bool {
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
	cluster, flow, unique := clusterWithFakeFlow(t, func(string) bool { return false })
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
