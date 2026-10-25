package run

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/stretchr/testify/require"
	"sync/atomic"
)

func TestNativeDeliveryDoesNotStartAfterCancellation(t *testing.T) {
	for _, reason := range []string{"parent", "stop", "superseded"} {
		t.Run(reason, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stop, superseded := make(chan struct{}), make(chan struct{})
			want := context.Canceled
			switch reason {
			case "parent":
				cancel()
			case "stop":
				close(stop)
				want = errRunStopped
			case "superseded":
				close(superseded)
				want = errSuperseded
			}
			calls := 0
			operation := func(context.Context) error { calls++; return nil }
			delivery := testDelivery(operation, operation)
			delivery.stop, delivery.superseded = stop, superseded
			require.ErrorIs(t, delivery.run(ctx), want)
			require.Zero(t, calls, "an abandoned delivery must reach neither readiness nor the host")
		})
	}
}

type blockedDeliveryFlow struct {
	*fakeLauncherFlow
	entered chan struct{}
	once    sync.Once
}

func (f *blockedDeliveryFlow) GRPCEndpointTarget(ctx context.Context, _, _, _ string) (orchestration.GRPCTarget, error) {
	f.once.Do(func() { close(f.entered) })
	<-ctx.Done()
	return orchestration.GRPCTarget{}, ctx.Err()
}

func TestNativeDeliveryCanCancelWhileAnotherDeliveryHoldsTheSerializer(t *testing.T) {
	cluster, flow, unique := launcherWithFakeFlow(t, func(string) bool { return true })
	blocked := &blockedDeliveryFlow{fakeLauncherFlow: flow, entered: make(chan struct{})}
	cluster.flow = blocked
	cluster.running[unique] = true
	first, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { firstDone <- cluster.deliver(first, "model") }()
	t.Cleanup(func() { cancelFirst(); <-firstDone })
	select {
	case <-blocked.entered:
	case err := <-firstDone:
		firstDone <- err
		t.Fatalf("first delivery did not reach its owner call: %v", err)
	case <-time.After(time.Second):
		t.Fatal("first delivery did not start")
	}

	second, cancelSecond := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelSecond()
	secondDone := make(chan error, 1)
	go func() { secondDone <- cluster.deliver(second, "model") }()
	select {
	case err := <-secondDone:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		// Release the first call so the broken baseline can finish too.
		cancelFirst()
		<-secondDone
		t.Fatal("canceled delivery is still waiting for another delivery's lock")
	}
}

// The lifecycle bound must reach the active owner call, not only its backoff.
// release lets even the broken baseline finish without leaving a goroutine.
func TestNativeDeliveryCancelsInFlightOperations(t *testing.T) {
	for _, phase := range []string{"readiness", "delivery"} {
		for _, reason := range []string{"parent", "stop", "superseded", "deadline"} {
			t.Run(phase+"/"+reason, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				stop, superseded := make(chan struct{}), make(chan struct{})
				entered, release := make(chan struct{}), make(chan struct{})
				var enterOnce sync.Once
				blocked := func(ctx context.Context) error {
					enterOnce.Do(func() { close(entered) })
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-release:
						return errors.New("test released blocked owner call")
					}
				}
				delivery := testDelivery(hostAlwaysReady, blocked)
				if phase == "readiness" {
					delivery.ready = blocked
					delivery.deliver = func(context.Context) error {
						t.Error("delivery started after readiness was canceled")
						return nil
					}
				}
				delivery.stop, delivery.superseded = stop, superseded
				if reason == "deadline" {
					delivery.schedule.Deadline = 50 * time.Millisecond
				}
				done := make(chan error, 1)
				go func() { done <- delivery.run(ctx) }()
				t.Cleanup(func() {
					close(release)
					cancel()
				})
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("owner call did not start")
				}
				want := context.Canceled
				switch reason {
				case "parent":
					cancel()
				case "stop":
					close(stop)
					want = errRunStopped
				case "superseded":
					close(superseded)
					want = errSuperseded
				case "deadline":
					want = context.DeadlineExceeded
				}
				select {
				case err := <-done:
					require.ErrorIs(t, err, want)
					if reason == "deadline" {
						require.ErrorContains(t, err, "configured bound 50ms")
						if phase == "readiness" {
							require.ErrorContains(t, err, "checking host readiness")
						} else {
							require.ErrorContains(t, err, "delivering the manifest")
						}
					}
				case <-time.After(time.Second):
					t.Error("active owner call outlived its delivery lifecycle")
				}
			})
		}
	}
}

// Capture the hook through the same registration used by attachLauncher. The
// probe waits for cancellation, while service teardown has not even begun.
type cancelableReadinessFlow struct {
	*fakeLauncherFlow
	entered  chan struct{}
	canceled chan struct{}
	calls    atomic.Int32
}

func (f *cancelableReadinessFlow) ServiceReadiness(ctx context.Context, service string) *orchestration.ReadinessFailure {
	if f.calls.Add(1) == 1 {
		close(f.entered)
	}
	<-ctx.Done()
	select {
	case f.canceled <- struct{}{}:
	default:
	}
	return &orchestration.ReadinessFailure{Service: service, Predicate: orchestration.PredicateLifecycle, Reason: ctx.Err().Error()}
}

func TestNativeDeliveryRunningHookRetainsRunCancellation(t *testing.T) {
	cluster, flow, unique := launcherWithFakeFlow(t, func(string) bool { return true })
	cluster.stop = make(chan struct{})
	blocked := &cancelableReadinessFlow{fakeLauncherFlow: flow, entered: make(chan struct{}), canceled: make(chan struct{}, 4)}
	cluster.flow = blocked
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); cluster.close() })
	var hook func(string, int)
	cluster.registerRunningHook(ctx, func(registered func(string, int)) { hook = registered })
	require.NotNil(t, hook)
	hook(unique, 0)
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("registered hook did not start readiness")
	}
	cancel()
	select {
	case <-blocked.canceled:
	case <-time.After(time.Second):
		t.Fatal("run cancellation did not reach delivery before service teardown")
	}
	// Shutdown has deliberately not signaled cluster.stop: parent cancellation
	// must suffice, even while service teardown would still be running.
	select {
	case <-cluster.stop:
		t.Fatal("test closed launcher before observing cancellation")
	default:
	}
	require.Eventually(t, func() bool {
		cluster.waitersMu.Lock()
		defer cluster.waitersMu.Unlock()
		return len(cluster.waiters) == 0
	}, time.Second, time.Millisecond)
	before := blocked.calls.Load()
	hook(unique, 0)
	require.Never(t, func() bool { return blocked.calls.Load() != before }, 30*time.Millisecond, time.Millisecond,
		"late Running must not start another readiness probe")
	require.True(t, deliveryAbandoned(context.Canceled), "run cancellation is expected abandonment")
}
