package gitops

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/codefly-dev/core/solutionhost"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// CompositionDelivery delivers through codefly.dev/composition: the signed
// carrier and the module's delivery credential go to ApplyManifest, which
// answers synchronously with the generation the host holds. The run then knows
// nothing of the host but the interface it declared.
type CompositionDelivery struct {
	client compositionv1.CompositionServiceClient

	mu      sync.Mutex
	applied map[string]uint64
}

// NewCompositionDelivery speaks the interface over conn.
func NewCompositionDelivery(conn grpc.ClientConnInterface) *CompositionDelivery {
	return &CompositionDelivery{client: compositionv1.NewCompositionServiceClient(conn), applied: map[string]uint64{}}
}

// Post is NativeDeliverer.Post over the interface.
func (c *CompositionDelivery) Post(ctx context.Context, module string, signed *solutionhost.Signed, token string) (Receipt, error) {
	out, err := c.client.ApplyManifest(ctx, &compositionv1.ApplyManifestRequest{
		Carrier:    &compositionv1.SignedManifestV1{Document: signed.Document, Envelope: signed.Envelope},
		Credential: token,
	})
	if err != nil {
		return Receipt{Status: httpStatusOf(err), Error: err.Error()}, &HostStatusError{Status: httpStatusOf(err),
			Message: fmt.Sprintf("the host refused the module manifest of %s through the composition interface: %v", module, err)}
	}
	manifest, err := solutionhost.ManifestFromVerified(signed.Document)
	if err != nil {
		return Receipt{}, err
	}
	c.mu.Lock()
	c.applied[manifest.Binding] = out.GetGeneration()
	c.mu.Unlock()
	return Receipt{Status: http.StatusOK, Disposition: "accepted", Generation: out.GetGeneration()}, nil
}

// Applied is NativeDeliverer.Applied over the interface: ApplyManifest applies
// synchronously, so what it answered is what the host holds.
func (c *CompositionDelivery) Applied(_ context.Context, binding string) (*appliedManifest, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	generation, ok := c.applied[binding]
	if !ok {
		return nil, false, nil
	}
	return &appliedManifest{Binding: binding, Generation: generation}, true, nil
}

// httpStatusOf maps a refusal's code onto the status the run's classification
// reads: a lower generation and a refused carrier are final, an unreachable host
// is transient.
func httpStatusOf(err error) int {
	switch status.Code(err) {
	case codes.FailedPrecondition:
		return http.StatusConflict
	case codes.NotFound:
		return http.StatusNotFound
	case codes.PermissionDenied, codes.Unauthenticated:
		return http.StatusForbidden
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.Unavailable, codes.DeadlineExceeded:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}
