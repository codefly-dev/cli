package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
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

// nativeFlow is what the native cluster reads from the run's flow.
type nativeFlow interface {
	World() *orchestration.World
	ServiceFromUnique(unique string) (*resources.Service, error)
	GetAddressForEndpoint(ctx context.Context, module, service, endpoint string) (string, error)
	StartedArtifact(unique string) *runtimev0.StartedArtifact
	EndpointDelivery(ctx context.Context, document *solutionhost.SolutionHostBinding, access *basev0.NetworkAccess) ([]byte, error)
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
	return errors.Is(err, errRunStopped) || errors.Is(err, errWorkloadGone) || errors.Is(err, errSuperseded)
}

// transientDeliveryRefusal reports whether a failed delivery attempt says the
// host is not serving yet rather than that it refuses: a 5xx (the envelope
// read's 503 among them); a 404 from the gateway, before it exposes the route
// (a 404 from the host's own delivery endpoint is a wrong endpoint, not a
// wait); a 409 on the envelope (a lost revision race); or no answer at all —
// refused, reset or timed out, never a request that could not be formed.
// Anything else — another 4xx, a policy refusal, an agent that reports no
// started artifact — is not, and fails at once.
func transientDeliveryRefusal(err error) bool {
	var refusal *gitops.HostStatusError
	if errors.As(err, &refusal) {
		switch {
		case refusal.Status >= http.StatusInternalServerError:
			return true
		case refusal.Status == http.StatusNotFound:
			return refusal.Gateway
		case refusal.Status == http.StatusConflict:
			return refusal.Envelope
		}
		return false
	}
	var transport *url.Error
	if !errors.As(err, &transport) {
		return false
	}
	var dial *net.OpError
	return transport.Timeout() || errors.As(transport.Err, &dial) ||
		errors.Is(transport.Err, syscall.ECONNRESET) || errors.Is(transport.Err, io.EOF) || errors.Is(transport.Err, io.ErrUnexpectedEOF)
}

// hostDelivery is one workload's delivery as the run attempts it.
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
func (d hostDelivery) run(ctx context.Context) error {
	deadline := time.Now().Add(d.schedule.Deadline)
	backoff := d.schedule.FirstBackoff
	var reason string
	var lastRefusal error
	for attempt := 1; ; attempt++ {
		err := d.ready(ctx)
		if errors.Is(err, errWorkloadGone) {
			return err
		}
		if err == nil {
			if err = d.deliver(ctx); err == nil {
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
			if lastRefusal != nil && lastRefusal != err {
				return fmt.Errorf("the host did not accept the delivery within %s: %w (its last refusal: %w)", d.schedule.Deadline, err, lastRefusal)
			}
			return fmt.Errorf("the host did not accept the delivery within %s: %w", d.schedule.Deadline, err)
		}
		if err.Error() != reason {
			reason = err.Error()
			d.log.Info("the delivery waits for the host", wool.Field("reason", reason))
		}
		d.log.Debug("retrying the delivery", wool.Field("attempt", attempt), wool.Field("in", backoff.String()), wool.ErrField(err))
		select {
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
func (cluster *nativeCluster) hostReady(ctx context.Context) error {
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
func (cluster *nativeCluster) waitFor(unique string) *deliveryWaiter {
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

func (cluster *nativeCluster) doneWaiting(unique string, waiter *deliveryWaiter) {
	cluster.waitersMu.Lock()
	defer cluster.waitersMu.Unlock()
	if cluster.waiters[unique] == waiter {
		delete(cluster.waiters, unique)
	}
}
