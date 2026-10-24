package gitops

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/codefly-dev/core/solutionhost"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// compositionStub is a host that implements codefly.dev/composition: it applies
// a manifest it has not seen at a higher generation and refuses a lower one by
// name, as the suite requires of every implementation.
type compositionStub struct {
	compositionv1.UnimplementedCompositionServiceServer
	held map[string]uint64
	seen []*compositionv1.ApplyManifestRequest
}

func (s *compositionStub) ApplyManifest(_ context.Context, req *compositionv1.ApplyManifestRequest) (*compositionv1.ApplyManifestResponse, error) {
	s.seen = append(s.seen, req)
	if req.GetCredential() == "" {
		return nil, status.Error(codes.PermissionDenied, "no delivery credential")
	}
	manifest, err := solutionhost.ManifestFromVerified(req.GetCarrier().GetDocument())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if manifest.Generation <= s.held[manifest.Binding] {
		return nil, status.Errorf(codes.FailedPrecondition, "generation %d does not move %s forward", manifest.Generation, manifest.Binding)
	}
	s.held[manifest.Binding] = manifest.Generation
	return &compositionv1.ApplyManifestResponse{Principal: manifest.Alias, Generation: manifest.Generation}, nil
}

func TestCompositionDeliveryCarriesTheSignedManifestThroughApplyManifest(t *testing.T) {
	stub := &compositionStub{held: map[string]uint64{}}
	server := grpc.NewServer()
	compositionv1.RegisterCompositionServiceServer(server, stub)
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	delivery := NewCompositionDelivery(conn, nil, nil, nil)
	manifest := &solutionhost.ModuleManifest{
		Schema: solutionhost.SchemaModuleManifestV1, Binding: "probe", Alias: "probe", Kind: solutionhost.KindModule, Generation: 2,
		Host: solutionhost.HostTarget{Coordinate: "local", Domain: "local"}, Release: solutionhost.Release{Publisher: "test", Name: "probe", Version: "0.1.0"},
		Workloads: []solutionhost.Workload{{Service: "api", Container: "app"}}, Authority: &solutionhost.Authority{Principal: "probe"},
	}
	document, err := solutionhost.SignedPayload(manifest)
	if err != nil {
		t.Fatal(err)
	}
	signed := &solutionhost.Signed{Document: document, Envelope: []byte(`{"envelope":"the test host does not verify"}`)}
	ctx := context.Background()
	receipt, err := delivery.Post(ctx, "probe", signed, "delivery-credential")
	if err != nil || receipt.Status != http.StatusOK || receipt.Generation != 2 || receipt.Disposition != "accepted" {
		t.Fatalf("post: %+v %v", receipt, err)
	}
	if stub.seen[0].GetCredential() != "delivery-credential" || string(stub.seen[0].GetCarrier().GetDocument()) != string(document) {
		t.Fatal("the carrier and the credential reach the host as sent")
	}
	if stub.seen[0].GetAuthority() != nil {
		t.Fatal("with no method source, no authority document is sent")
	}
	applied, found, err := delivery.Applied(ctx, "probe")
	if err != nil || !found || applied.Generation != 2 {
		t.Fatalf("applied: %+v %v %v", applied, found, err)
	}
	if _, found, _ := delivery.Applied(ctx, "never-delivered"); found {
		t.Fatal("an undelivered binding is not applied")
	}
	manifest.Generation = 1
	document, _ = solutionhost.SignedPayload(manifest)
	_, err = delivery.Post(ctx, "probe", &solutionhost.Signed{Document: document, Envelope: signed.Envelope}, "delivery-credential")
	var refusal *HostStatusError
	if !asHostStatus(err, &refusal) || refusal.Status != http.StatusConflict {
		t.Fatalf("a lower generation is a final refusal (409): %v", err)
	}
	_, err = delivery.Post(ctx, "probe", signed, "")
	if !asHostStatus(err, &refusal) || refusal.Status != http.StatusForbidden {
		t.Fatalf("no credential is forbidden (403): %v", err)
	}
}

func asHostStatus(err error, target **HostStatusError) bool {
	for err != nil {
		if e, ok := err.(*HostStatusError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
