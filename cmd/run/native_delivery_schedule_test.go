package run

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/stretchr/testify/require"
)

var testDeliverySchedule = deliverySchedule{Deadline: 2 * time.Second, FirstBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}

func hostAlwaysReady(context.Context) error { return nil }

// fixtureGateway is a host gateway that signs the fixture administrator in
// and answers the module-envelope read with status(n) on its n-th read.
func fixtureGateway(t *testing.T, status func(read int64) int, body string) (*gitops.EnvelopeWriter, *atomic.Int64) {
	t.Helper()
	var reads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/auth/authenticate":
			_, _ = response.Write([]byte(`{"accessToken":"admin"}`))
		case "/v1/platform/module-envelope":
			response.WriteHeader(status(reads.Add(1)))
			_, _ = response.Write([]byte(body))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	return &gitops.EnvelopeWriter{GatewayURL: server.URL, FixtureProvider: "fixture", FixtureToken: "admin"}, &reads
}

// planThrough is the first host call a delivery makes, wrapped as deliver
// wraps it.
func planThrough(writer *gitops.EnvelopeWriter) func(context.Context) error {
	return func(ctx context.Context) error {
		if _, _, err := writer.Current(ctx); err != nil {
			return fmt.Errorf("plan the module envelope: %w", err)
		}
		return nil
	}
}

// The shape of the platform-core run: the gateway listens but answers the
// envelope read with 503 until it has configured JWT validation. The delivery
// asks again and is delivered, rather than giving up on the first answer.
func TestANativeDeliveryRefusedWith503IsRetriedUntilAccepted(t *testing.T) {
	writer, reads := fixtureGateway(t, func(read int64) int {
		if read <= 2 {
			return http.StatusServiceUnavailable
		}
		return http.StatusNotFound // no envelope written yet: a valid answer
	}, "JWT validation not configured")

	err := deliverWhenHostReady(context.Background(), nil, testDeliverySchedule, hostAlwaysReady, planThrough(writer))
	require.NoError(t, err)
	require.Equal(t, int64(3), reads.Load(), "two 503s, then the answer that lets the delivery proceed")
}

// A refusal that says no is not waited out: it fails once, with its reason.
func TestANativeDeliveryRefusedForGoodIsNotRetried(t *testing.T) {
	writer, reads := fixtureGateway(t, func(int64) int { return http.StatusForbidden }, "not a platform administrator")
	err := deliverWhenHostReady(context.Background(), nil, testDeliverySchedule, hostAlwaysReady, planThrough(writer))
	require.ErrorContains(t, err, "HTTP 403: not a platform administrator")
	require.Equal(t, int64(1), reads.Load())

	// Nor is a refusal the host never gave: an agent that reports no
	// started artifact is the module's to fix, not the host's to wait for.
	attempts := 0
	err = deliverWhenHostReady(context.Background(), nil, testDeliverySchedule, hostAlwaysReady, func(context.Context) error {
		attempts++
		return errors.New("agent codefly.dev/go-grpc:0.1.55 reports no started artifact (runtime-started-artifact/v1)")
	})
	require.ErrorContains(t, err, "reports no started artifact")
	require.Equal(t, 1, attempts)
}

// A host that never serves the delivery is reported at the deadline with the
// last thing it answered, so the person sees why the module is not routed.
func TestANativeDeliveryStillRefusedAtTheDeadlineNamesTheLastStatus(t *testing.T) {
	writer, _ := fixtureGateway(t, func(int64) int { return http.StatusServiceUnavailable }, "JWT validation not configured")
	schedule := deliverySchedule{Deadline: 30 * time.Millisecond, FirstBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}
	err := deliverWhenHostReady(context.Background(), nil, schedule, hostAlwaysReady, planThrough(writer))
	require.ErrorContains(t, err, "did not accept the delivery within 30ms")
	require.ErrorContains(t, err, "HTTP 503: JWT validation not configured")
}

// A module that reports Running before the gateway is ready is delivered once
// the gateway is, and not before: the delivery is never attempted against a
// host that is not serving.
func TestANativeDeliveryOfAModuleRunningBeforeTheGatewayWaitsForTheGateway(t *testing.T) {
	var gatewayReady atomic.Bool
	checks := 0
	hostReady := func(context.Context) error {
		checks++
		if checks == 3 {
			gatewayReady.Store(true) // the gateway finishes starting
		}
		if !gatewayReady.Load() {
			return errors.New("the host's saas/auth-gateway is not ready")
		}
		return nil
	}
	var deliveredBeforeReady, delivered bool
	err := deliverWhenHostReady(context.Background(), nil, testDeliverySchedule, hostReady, func(context.Context) error {
		deliveredBeforeReady = deliveredBeforeReady || !gatewayReady.Load()
		delivered = true
		return nil
	})
	require.NoError(t, err)
	require.True(t, delivered)
	require.False(t, deliveredBeforeReady)
	require.Equal(t, 3, checks)

	// The run's own answer: a flow in which the gateway has not reached
	// Running names the gateway as what the delivery waits for.
	cluster := &nativeCluster{
		flow: &orchestration.Flow{}, hostModule: "saas", hostService: "accounts",
		env: &environments.Environment{Host: &environments.EnvironmentHost{Gateway: "saas/auth-gateway"}},
	}
	require.ErrorContains(t, cluster.hostReady(context.Background()), "saas/auth-gateway is not ready")
}

// The run stopping ends a waiting delivery quietly.
func TestANativeDeliveryWaitingWhenTheRunStopsReportsNothing(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	err := deliverWhenHostReady(context.Background(), stop, testDeliverySchedule, func(context.Context) error {
		return errors.New("not ready")
	}, func(context.Context) error { return nil })
	require.ErrorIs(t, err, errRunStopped)
}
