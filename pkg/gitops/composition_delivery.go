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
	"google.golang.org/protobuf/proto"

	delivery "github.com/codefly-dev/authority/libraries/delivery/go"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// CompositionDelivery delivers through codefly.dev/composition: the signed
// carrier and the module's delivery credential go to ApplyManifest, which
// answers synchronously with the generation the host holds. The run then knows
// nothing of the host but the interface it declared.
type CompositionDelivery struct {
	client compositionv1.CompositionServiceClient
	// signer is the run's release key, which signs the core manifest too.
	signer solutionhost.Signer
	// methods reads what each method of a module declares, from its package;
	// ok false means the module declares no method policy.
	methods func(ctx context.Context, module string) ([]*compositionv1.MethodPolicyV1, bool, error)

	mu      sync.Mutex
	applied map[string]uint64
}

// NewCompositionDelivery speaks the interface over conn. With each manifest it
// delivers the authority's own document, signed by signer: what each method of
// the module declares, read by methods from the module's package.
func NewCompositionDelivery(conn grpc.ClientConnInterface, signer solutionhost.Signer,
	methods func(ctx context.Context, module string) ([]*compositionv1.MethodPolicyV1, bool, error)) *CompositionDelivery {
	return &CompositionDelivery{client: compositionv1.NewCompositionServiceClient(conn), signer: signer, methods: methods, applied: map[string]uint64{}}
}

// Post is NativeDeliverer.Post over the interface.
func (c *CompositionDelivery) Post(ctx context.Context, module string, signed *solutionhost.Signed, token string) (Receipt, error) {
	manifest, err := solutionhost.ManifestFromVerified(signed.Document)
	if err != nil {
		return Receipt{}, err
	}
	request := &compositionv1.ApplyManifestRequest{
		Carrier:    &compositionv1.SignedManifestV1{Document: signed.Document, Envelope: signed.Envelope},
		Credential: token,
	}
	if request.Authority, err = c.authority(ctx, module, manifest, signed.Document); err != nil {
		return Receipt{}, err
	}
	out, err := c.client.ApplyManifest(ctx, request)
	if err != nil {
		return Receipt{Status: httpStatusOf(err), Error: err.Error()}, &HostStatusError{Status: httpStatusOf(err),
			Message: fmt.Sprintf("the host refused the module manifest of %s through the composition interface: %v", module, err)}
	}
	c.mu.Lock()
	c.applied[manifest.Binding] = out.GetGeneration()
	c.mu.Unlock()
	return Receipt{Status: http.StatusOK, Disposition: "accepted", Generation: out.GetGeneration()}, nil
}

// authority signs the authority's document for one delivery: the module's
// method policies, bound to the core manifest by its module, generation and
// digest, under the key that signs the manifest. Nil when the module declares
// no method policy.
func (c *CompositionDelivery) authority(ctx context.Context, module string, manifest *solutionhost.ModuleManifest, core []byte) (*compositionv1.SignedManifestV1, error) {
	if c.methods == nil || c.signer == nil {
		return nil, nil
	}
	methods, ok, err := c.methods(ctx, module)
	if err != nil || !ok {
		return nil, err
	}
	document, err := proto.MarshalOptions{Deterministic: true}.Marshal(&compositionv1.AuthorityManifestV1{
		Module: manifest.Alias, Generation: manifest.Generation, CoreManifestDigest: delivery.Digest(core), Methods: methods,
	})
	if err != nil {
		return nil, err
	}
	envelope, err := delivery.Sign(document, c.signer.KeyID(), c.signer.Sign)
	if err != nil {
		return nil, err
	}
	return &compositionv1.SignedManifestV1{Document: document, Envelope: envelope}, nil
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
