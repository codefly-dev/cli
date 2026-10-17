package run

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/core/wool"
)

// A workload reports Running on its own schedule, which is not the host's: in
// a composition, a module the gateway does not depend on can be Running long
// before the gateway answers, and a gateway that listens can still answer 503
// until it has read its own configuration. So a native delivery waits for the
// host — its gateway and its delivery service, by the readiness the run gates
// on (Flow.ServiceReady) — and retries a refusal that only says the host is
// not serving yet, until a deadline. A refusal that says no is never retried.

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

// errRunStopped ends a delivery that was still waiting when the run stopped:
// nothing went wrong, so nothing is reported.
var errRunStopped = errors.New("the run stopped before the delivery")

// transientDeliveryRefusal reports whether a failed delivery attempt says the
// host is not serving yet rather than that it refuses: a 5xx (the envelope
// read's 503 among them), a 404 before the gateway exposes the route, a 409
// on the envelope (a lost revision race), or no answer at all (connection
// refused or reset). Anything else — another 4xx, a policy refusal, an agent
// that reports no started artifact — is not, and fails at once.
func transientDeliveryRefusal(err error) bool {
	var refusal *gitops.HostStatusError
	if errors.As(err, &refusal) {
		return refusal.Status >= http.StatusInternalServerError ||
			refusal.Status == http.StatusNotFound ||
			(refusal.Envelope && refusal.Status == http.StatusConflict)
	}
	var transport *url.Error
	return errors.As(err, &transport)
}

// deliverWhenHostReady runs deliver once hostReady holds, retrying while the
// host is not ready or answers with a transient refusal, with a backoff that
// doubles up to MaxBackoff, until Deadline. Each retry is logged at debug. A
// non-transient refusal is returned at once; at the deadline the last reason
// is returned. stop ends the wait with errRunStopped.
func deliverWhenHostReady(ctx context.Context, stop <-chan struct{}, schedule deliverySchedule, hostReady, deliver func(context.Context) error) error {
	w := wool.Get(ctx).In("nativeCluster.deliver")
	deadline := time.Now().Add(schedule.Deadline)
	backoff := schedule.FirstBackoff
	for attempt := 1; ; attempt++ {
		err := hostReady(ctx)
		if err == nil {
			if err = deliver(ctx); err == nil {
				return nil
			}
			if !transientDeliveryRefusal(err) {
				return err
			}
		}
		if !time.Now().Add(backoff).Before(deadline) {
			return fmt.Errorf("the host did not accept the delivery within %s: %w", schedule.Deadline, err)
		}
		w.Debug("the host is not serving the delivery yet; retrying", wool.Field("attempt", attempt), wool.Field("in", backoff.String()), wool.ErrField(err))
		select {
		case <-stop:
			return errRunStopped
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, schedule.MaxBackoff)
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
		if !cluster.flow.ServiceReady(ctx, unique) {
			return fmt.Errorf("the host's %s is not ready", unique)
		}
	}
	return nil
}
