package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"syscall"
	"time"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// A workload reports Running on its own schedule, which is not the host's: in
// a composition, a module the gateway does not depend on can be Running long
// before the gateway answers, and a gateway that listens can still answer 503
// until it has read its own configuration. So a native delivery waits for the
// host — its gateway and its delivery service, by the readiness the run gates
// on (Flow.ServiceReadiness) — and retries a refusal that only says the host
// is not serving yet, until a deadline. A refusal that says no is never
// retried.

// launcherFlow is what the launcher reads from the run's flow.
type launcherFlow interface {
	World() *orchestration.World
	ServiceFromUnique(unique string) (*resources.Service, error)
	GetAddressForEndpoint(ctx context.Context, module, service, endpoint string) (string, error)
	GRPCEndpointTarget(ctx context.Context, module, service, endpoint string) (orchestration.GRPCTarget, error)
	ServiceRuntimeContext(unique string) string
	EndpointCarriers(ctx context.Context, module string, access *basev0.NetworkAccess) (map[string]string, error)
	ServiceReadiness(ctx context.Context, service string) *orchestration.ReadinessFailure
}

// deliverySchedule bounds how long one workload's delivery waits for the host
// and how often it asks again.
type deliverySchedule struct {
	Deadline     time.Duration
	FirstBackoff time.Duration
	MaxBackoff   time.Duration
}

// nativeDeliverySchedule is a run's: long enough for the slowest gateway of a
// full composition to boot behind its own dependencies, short enough that a
// module that will never be delivered is reported while the person is still
// looking at the run.
var nativeDeliverySchedule = deliverySchedule{Deadline: 10 * time.Minute, FirstBackoff: 500 * time.Millisecond, MaxBackoff: 10 * time.Second}

// A delivery abandoned for one of these reasons is not a failure, and is not
// reported as one.
var (
	errRunStopped   = errors.New("the run stopped before the delivery")
	errWorkloadGone = errors.New("the workload left the composition before its delivery")
	errSuperseded   = errors.New("the workload started again; its new delivery replaces this one")
)

func deliveryAbandoned(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, errRunStopped) || errors.Is(err, errWorkloadGone) || errors.Is(err, errSuperseded)
}

// transientDeliveryRefusal reports whether a failed delivery attempt says the
// host is not serving yet rather than that it refuses: the host was
// unreachable, out of time or out of capacity (HostStatusError.Transient, which
// also classifies the legacy HTTP delivery's statuses), or the HTTP request got
// no answer at all.
// Anything else — an interface the host does not serve, a policy refusal, a
// manifest the host refuses, a refusal it could not classify — is its answer,
// and fails at once with its reason.
func transientDeliveryRefusal(err error) bool {
	var refusal *gitops.HostStatusError
	if errors.As(err, &refusal) {
		return refusal.Transient()
	}
	// The legacy HTTP delivery's no answer at all: refused, reset or timed
	// out, never a request that could not be formed.
	var transport *url.Error
	if !errors.As(err, &transport) {
		return false
	}
	var dial *net.OpError
	return transport.Timeout() || errors.As(transport.Err, &dial) ||
		errors.Is(transport.Err, syscall.ECONNRESET) || errors.Is(transport.Err, io.EOF) || errors.Is(transport.Err, io.ErrUnexpectedEOF)
}

// hostDelivery is one workload's delivery as the run attempts it: its
// module's manifest, listing it.
type hostDelivery struct {
	schedule deliverySchedule
	// stop is the run's; superseded is closed when the workload starts again.
	stop, superseded <-chan struct{}
	// ready answers nil once the host is ready, why it is not otherwise, and
	// errWorkloadGone once the workload has left the composition.
	ready   func(context.Context) error
	deliver func(context.Context) error
	log     *wool.Wool
}

// run delivers once ready holds, retrying while the host is not ready or
// answers with a transient refusal, with a backoff that doubles up to
// MaxBackoff, until Deadline. The first wait and every change of its reason
// are logged at info, each retry at debug. A non-transient refusal is
// returned at once; at the deadline, the reason the host is still not
// serving and the last refusal it answered.
func (d hostDelivery) run(parent context.Context) (result error) {
	bounded, timeout := context.WithTimeout(parent, d.schedule.Deadline)
	defer timeout()
	ctx, cancel := context.WithCancelCause(bounded)
	defer cancel(nil)
	// Stop and supersession must reach a readiness probe or delivery RPC already
	// in flight, not just the wait between attempts. The owned calls honor ctx.
	go func() {
		select {
		case <-d.stop:
			cancel(errRunStopped)
		case <-d.superseded:
			cancel(errSuperseded)
		case <-ctx.Done():
		}
	}()
	cancellation := func() error {
		// Check the lifecycle channels synchronously too: a preclosed channel
		// must prevent the first call without waiting for the watcher to run.
		select {
		case <-d.stop:
			cancel(errRunStopped)
		case <-d.superseded:
			cancel(errSuperseded)
		default:
		}
		return context.Cause(ctx)
	}
	deadline, _ := ctx.Deadline()
	backoff := d.schedule.FirstBackoff
	var reason string
	var lastRefusal error
	phase := "checking host readiness"
	defer func() {
		if errors.Is(result, context.DeadlineExceeded) {
			result = fmt.Errorf("delivery completion was not confirmed before its deadline (configured bound %s; %s; last wait reason: %q): %w", d.schedule.Deadline, phase, reason, result)
			if lastRefusal != nil {
				result = fmt.Errorf("%w (its last refusal: %w)", result, lastRefusal)
			}
		}
	}()
	for attempt := 1; ; attempt++ {
		if err := cancellation(); err != nil {
			return err
		}
		phase = "checking host readiness"
		err := d.ready(ctx)
		if cause := cancellation(); cause != nil {
			return cause
		}
		if errors.Is(err, errWorkloadGone) {
			return err
		}
		if err == nil {
			phase = "delivering the manifest"
			err = d.deliver(ctx)
			if cause := cancellation(); cause != nil {
				return cause
			}
			if err == nil {
				if attempt > 1 {
					d.log.Info("delivered once the host was serving", wool.Field("attempts", attempt))
				}
				return nil
			}
			if !transientDeliveryRefusal(err) {
				return err
			}
			lastRefusal = err
		}
		if !time.Now().Add(backoff).Before(deadline) {
			return errors.Join(context.DeadlineExceeded, err)
		}
		if err.Error() != reason {
			reason = err.Error()
			d.log.Info("the delivery waits for the host", wool.Field("reason", reason))
		}
		d.log.Debug("retrying the delivery", wool.Field("attempt", attempt), wool.Field("in", backoff.String()), wool.ErrField(err))
		select {
		case <-ctx.Done():
			return cancellation()
		case <-d.stop:
			return errRunStopped
		case <-d.superseded:
			return errSuperseded
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, d.schedule.MaxBackoff)
	}
}

// hostReady reports which of the host's services, if any, is not ready yet:
// the gateway the envelope is written through, then the service whose
// delivery endpoint receives the documents.
func (cluster *launcher) hostReady(ctx context.Context) error {
	for _, unique := range []string{cluster.env.Host.Gateway, cluster.hostModule + "/" + cluster.hostService} {
		if unique == "" {
			continue
		}
		if failure := cluster.flow.ServiceReadiness(ctx, unique); failure != nil {
			return fmt.Errorf("the host is not ready: %s", failure)
		}
	}
	return nil
}

// deliveryWaiter is the one delivery a workload has in flight.
type deliveryWaiter struct{ superseded chan struct{} }

// waitFor registers a workload's delivery, superseding the one a previous
// start left waiting: a reload starts the workload again, and only its newest
// start is delivered.
func (cluster *launcher) waitFor(unique string) *deliveryWaiter {
	cluster.waitersMu.Lock()
	defer cluster.waitersMu.Unlock()
	if previous, ok := cluster.waiters[unique]; ok {
		close(previous.superseded)
	}
	if cluster.waiters == nil {
		cluster.waiters = map[string]*deliveryWaiter{}
	}
	waiter := &deliveryWaiter{superseded: make(chan struct{})}
	cluster.waiters[unique] = waiter
	return waiter
}

func (cluster *launcher) doneWaiting(unique string, waiter *deliveryWaiter) {
	cluster.waitersMu.Lock()
	defer cluster.waitersMu.Unlock()
	if cluster.waiters[unique] == waiter {
		delete(cluster.waiters, unique)
	}
}
