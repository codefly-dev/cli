package gitops

import (
	"context"
	"errors"
	"fmt"
	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	"net"
	"strings"
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
	held            map[string]uint64
	seen            []*compositionv1.ApplyManifestRequest
	replyGeneration uint64
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
	generation := manifest.Generation
	if s.replyGeneration != 0 {
		generation = s.replyGeneration
	}
	return &compositionv1.ApplyManifestResponse{Principal: manifest.Alias, Generation: generation}, nil
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
	delivery := NewCompositionDelivery(conn, nil, nil, nil, nil, nil)
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
	receipt, err := send(ctx, delivery, "probe", signed, "delivery-credential")
	if err != nil || receipt.Generation != 2 || receipt.Disposition != "accepted" {
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
	_, err = send(ctx, delivery, "probe", &solutionhost.Signed{Document: document, Envelope: signed.Envelope}, "delivery-credential")
	var refusal *HostStatusError
	if !asHostStatus(err, &refusal) || refusal.Code != codes.FailedPrecondition || refusal.Transient() {
		t.Fatalf("a lower generation is a final refusal: %v", err)
	}
	_, err = send(ctx, delivery, "probe", signed, "")
	if !asHostStatus(err, &refusal) || refusal.Code != codes.PermissionDenied || refusal.Transient() {
		t.Fatalf("no credential is a final refusal: %v", err)
	}
}

// send prepares the module's declaration and sends signed with it, as
// NativeDeliverer.Deliver does once the generation is settled.
func send(ctx context.Context, c *CompositionDelivery, module string, signed *solutionhost.Signed, token string) (Receipt, error) {
	manifest, err := solutionhost.ManifestFromVerified(signed.Document)
	if err != nil {
		return Receipt{}, err
	}
	prepared, err := c.Prepare(ctx, module, manifest.Removed)
	if err != nil {
		return Receipt{}, err
	}
	return prepared.Post(ctx, module, signed, token)
}

// TestOnlyAHostThatIsNotServingIsRetried: a refusal is the host's answer and
// fails at once, whatever its code — an interface it does not serve, an
// internal failure, a bad request, a denial, a precondition — and only
// unavailability, a deadline or exhausted capacity says to ask again.
func TestOnlyAHostThatIsNotServingIsRetried(t *testing.T) {
	for code, transient := range map[codes.Code]bool{
		codes.Unavailable: true, codes.DeadlineExceeded: true, codes.ResourceExhausted: true,
		codes.Unimplemented: false, codes.Internal: false, codes.InvalidArgument: false, codes.PermissionDenied: false,
		codes.FailedPrecondition: false, codes.NotFound: false, codes.Unknown: false, codes.Aborted: false,
	} {
		refusal, ok := refusal("probe", &solutionhost.ModuleManifest{Binding: "b", Generation: 2}, status.Error(code, "no")).(*HostStatusError)
		if !ok || refusal.Code != code || refusal.Transient() != transient {
			t.Fatalf("%s: transient %v, want %v", code, refusal.Transient(), transient)
		}
	}
}

// TestAHostHoldingAWithdrawalSaysWhatToDo: the binding is derived, so the
// refusal of a re-added module names the one thing the person can do.
func TestAHostHoldingAWithdrawalSaysWhatToDo(t *testing.T) {
	err := refusal("crm", &solutionhost.ModuleManifest{Binding: "b-crm", Generation: 3},
		status.Errorf(codes.FailedPrecondition, "stale: %v: binding \"b-crm\" was withdrawn at generation 2", solutionhost.ErrWithdrawn))
	var held *HostStatusError
	if !asHostStatus(err, &held) || held.Transient() {
		t.Fatalf("a held withdrawal is a final refusal: %v", err)
	}
	for _, want := range []string{"binding b-crm", "generation 3", "stop the run and start it again"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
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

func TestCompositionWithdrawalDoesNotReadOrDeliverLivePolicies(t *testing.T) {
	for _, kind := range []solutionhost.Kind{solutionhost.KindModule, solutionhost.KindSolution} {
		t.Run(string(kind), func(t *testing.T) {
			host := &compositionStub{held: map[string]uint64{}}
			server := grpc.NewServer()
			compositionv1.RegisterCompositionServiceServer(server, host)
			listener := bufconn.Listen(1 << 20)
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(server.Stop)
			conn, err := grpc.NewClient("passthrough:///bufconn", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			sourceReads := 0
			missing := func() error { sourceReads++; return errors.New("removed module source no longer available") }
			signer := nativeSigner(t)
			delivery := NewCompositionDelivery(conn, signer,
				func(context.Context, string) ([]*compositionv1.MethodPolicyV1, bool, error) {
					return nil, false, missing()
				},
				func(context.Context, string) ([]*authorityv1.WorkScopeV1, error) { return nil, missing() },
				func(context.Context, string) ([]*compositionv1.RoleV1, error) { return nil, missing() },
				func(context.Context, string) (bool, error) { return false, missing() })
			manifest := &solutionhost.ModuleManifest{Schema: solutionhost.SchemaModuleManifestV1, Binding: "probe", Alias: "probe", Kind: kind, Generation: 2, Removed: true,
				Host: solutionhost.HostTarget{Coordinate: "local", Domain: "local"}, Release: solutionhost.Release{Publisher: "test", Name: "probe", Version: "0.1.0"}}
			payload, err := solutionhost.SignedPayload(manifest)
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := solutionhost.SignEnvelope(payload, signer)
			if err != nil {
				t.Fatal(err)
			}
			signed, err := solutionhost.Carrier(payload, envelope)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := send(context.Background(), delivery, "probe", signed, "delivery-credential")
			if err != nil {
				t.Fatal(err)
			}
			native := &NativeDeliverer{Applied: delivery.Applied}
			if err := native.WaitForWithdrawal(t.Context(), manifest); err != nil {
				t.Fatalf("interface receipt did not prove withdrawal: %v", err)
			}
			if receipt.Generation != 2 || sourceReads != 0 || len(host.seen) != 1 || host.seen[0].Authority != nil {
				t.Fatalf("withdrawal depends on live policy: reads=%d receipt=%+v", sourceReads, receipt)
			}
		})
	}
}

func TestCompositionDeliveryRefusesAMismatchedAcknowledgement(t *testing.T) {
	for _, generation := range []uint64{1, 3} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			host := &compositionStub{held: map[string]uint64{}, replyGeneration: generation}
			server := grpc.NewServer()
			compositionv1.RegisterCompositionServiceServer(server, host)
			listener := bufconn.Listen(1 << 20)
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(server.Stop)
			conn, err := grpc.NewClient("passthrough:///bufconn", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			delivery := NewCompositionDelivery(conn, nil, nil, nil, nil, nil)
			manifest := &solutionhost.ModuleManifest{Schema: solutionhost.SchemaModuleManifestV1, Binding: "probe", Alias: "probe", Kind: solutionhost.KindModule, Generation: 2, Removed: true, Host: solutionhost.HostTarget{Coordinate: "local", Domain: "local"}, Release: solutionhost.Release{Publisher: "test", Name: "probe", Version: "0.1.0"}}
			document, err := solutionhost.SignedPayload(manifest)
			if err != nil {
				t.Fatal(err)
			}
			_, err = send(t.Context(), delivery, "probe", &solutionhost.Signed{Document: document, Envelope: []byte(`{"test":true}`)}, "credential")
			if err == nil {
				t.Fatal("wrong generation was treated as applied")
			}
			if _, found, err := delivery.Applied(t.Context(), "probe"); err != nil || found {
				t.Fatalf("mismatched reply populated applied record: %v %v", found, err)
			}
		})
	}
}
