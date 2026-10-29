package gitops

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// applyHost is a host that records exactly what reached ApplyManifest and can
// be told to answer the way a real one does: not serving yet, or its own
// refusal. It is a real gRPC server over bufconn with the generated client in
// front of it, so what these tests exercise is the wire the Job uses.
type applyHost struct {
	compositionv1.UnimplementedCompositionServiceServer
	seen []*compositionv1.ApplyManifestRequest
	// answers is consumed one per call; a nil entry accepts.
	answers []error
}

func (h *applyHost) ApplyManifest(_ context.Context, request *compositionv1.ApplyManifestRequest) (*compositionv1.ApplyManifestResponse, error) {
	h.seen = append(h.seen, proto.Clone(request).(*compositionv1.ApplyManifestRequest))
	if len(h.answers) > 0 {
		answer := h.answers[0]
		h.answers = h.answers[1:]
		if answer != nil {
			return nil, answer
		}
	}
	manifest, err := solutionhost.ManifestFromVerified(request.GetCarrier().GetDocument())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &compositionv1.ApplyManifestResponse{Principal: manifest.Alias, Generation: manifest.Generation}, nil
}

func applyHostClient(t *testing.T, host compositionv1.CompositionServiceServer) compositionv1.CompositionServiceClient {
	t.Helper()
	server := grpc.NewServer()
	compositionv1.RegisterCompositionServiceServer(server, host)
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	connection, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	return compositionv1.NewCompositionServiceClient(connection)
}

// preparedDocumentOf writes one prepared paired request: the Core carrier and
// the authority document beside it, and NO credential — the shape a render
// writes and the sender completes at execution.
func preparedDocumentOf(t *testing.T, directory, name, binding string, generation uint64, authority []byte) {
	t.Helper()
	manifest := &solutionhost.ModuleManifest{
		Schema: solutionhost.SchemaModuleManifestV1, Binding: binding, Alias: binding, Kind: solutionhost.KindModule, Generation: generation,
		Host:      solutionhost.HostTarget{Coordinate: "local", Domain: "local"},
		Release:   solutionhost.Release{Publisher: "test", Name: binding, Version: "0.1.0"},
		Workloads: []solutionhost.Workload{{Service: "api", Container: "app"}},
		Authority: &solutionhost.Authority{Principal: binding},
	}
	document, err := solutionhost.SignedPayload(manifest)
	require.NoError(t, err)
	request := &compositionv1.ApplyManifestRequest{
		Carrier: &compositionv1.SignedManifestV1{Document: document, Envelope: []byte(`{"envelope":"` + binding + `"}`)},
	}
	if authority != nil {
		request.Authority = &compositionv1.SignedManifestV1{Document: authority, Envelope: []byte(`{"envelope":"authority"}`)}
	}
	data, err := protojson.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, name), data, 0o600))
}

func noPause(context.Context, time.Duration) error { return nil }

// The whole point of the runner: both signed halves reach the host, and the
// credential is not in the document — it is read from the projection at
// execution, once per request, raw.
func TestTheRunnerSendsBothSignedHalvesWithACredentialReadAtExecution(t *testing.T) {
	directory := t.TempDir()
	preparedDocumentOf(t, directory, "a-billing.json", "billing", 3, []byte("authority-of-billing"))
	preparedDocumentOf(t, directory, "b-shop.json", "shop", 1, []byte("authority-of-shop"))
	host := &applyHost{}
	reads := 0
	sender := &PairedApply{
		Client: applyHostClient(t, host),
		Credential: func() (string, error) {
			reads++
			return "projected-token", nil
		},
		Sleep: noPause,
	}
	receipts, err := sender.Apply(context.Background(), directory)
	require.NoError(t, err)
	require.Len(t, receipts, 2)
	require.True(t, receipts[0].Accepted)
	require.True(t, receipts[1].Accepted)
	require.Equal(t, []string{"billing", "shop"}, []string{receipts[0].Binding, receipts[1].Binding},
		"documents are sent in name order, which is the render's order")
	require.Len(t, host.seen, 2)
	for i, want := range []string{"authority-of-billing", "authority-of-shop"} {
		require.NotNil(t, host.seen[i].GetCarrier(), "the Core signed carrier")
		require.Equal(t, want, string(host.seen[i].GetAuthority().GetDocument()),
			"and the authority document beside it — the half the legacy HTTP route cannot carry")
		require.Equal(t, "projected-token", host.seen[i].GetCredential(),
			"raw, with no bearer scheme: the host adds it before authorizing the carrier")
	}
	require.Equal(t, 2, reads, "read once per REQUEST, not once per run, so a rotated projection is re-read")
}

// A credential in a rendered document would be a published secret with a
// lifetime of minutes. It is refused where it is read, not sent and refused at
// the host.
func TestAPreparedDocumentHoldingACredentialIsRefusedRatherThanSent(t *testing.T) {
	directory := t.TempDir()
	request := &compositionv1.ApplyManifestRequest{
		Carrier:    &compositionv1.SignedManifestV1{Document: []byte("{}"), Envelope: []byte("{}")},
		Credential: "a-token-that-was-rendered",
	}
	data, err := protojson.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "leaky.json"), data, 0o600))
	host := &applyHost{}
	sender := &PairedApply{Client: applyHostClient(t, host), Credential: func() (string, error) { return "t", nil }, Sleep: noPause}
	_, err = sender.Apply(context.Background(), directory)
	require.ErrorContains(t, err, "carries a credential")
	require.Empty(t, host.seen, "nothing is sent once a document is refused")
}

// A refusal the host MEANS is its answer, and asking again gets the same one.
// FailedPrecondition is how core's terminal withdrawal and a stale generation
// arrive, and it is the status a Connect transport renders as 412 — the one the
// shell runner retried until its budget was spent.
func TestTheHostsOwnRefusalIsAnsweredOnceAndNotRetried(t *testing.T) {
	directory := t.TempDir()
	preparedDocumentOf(t, directory, "one.json", "billing", 2, []byte("authority"))
	host := &applyHost{answers: []error{status.Error(codes.FailedPrecondition, "generation 2 does not move billing forward")}}
	sender := &PairedApply{
		Client: applyHostClient(t, host), Credential: func() (string, error) { return "t", nil },
		Sleep: noPause, Budget: time.Hour,
	}
	receipts, err := sender.Apply(context.Background(), directory)
	require.ErrorContains(t, err, "one.json")
	require.Len(t, host.seen, 1, "asked once; the host's answer is not a reason to ask again")
	require.False(t, receipts[0].Accepted)
	require.ErrorContains(t, receipts[0].Err, "does not move billing forward")
}

// "Not serving yet" is the one answer worth asking again for.
func TestNotServingYetIsRetriedUntilTheHostAnswers(t *testing.T) {
	directory := t.TempDir()
	preparedDocumentOf(t, directory, "one.json", "billing", 2, []byte("authority"))
	host := &applyHost{answers: []error{
		status.Error(codes.Unavailable, "not serving yet"),
		status.Error(codes.DeadlineExceeded, "out of time"),
		nil,
	}}
	sender := &PairedApply{
		Client: applyHostClient(t, host), Credential: func() (string, error) { return "t", nil },
		Sleep: noPause, Budget: time.Hour,
	}
	receipts, err := sender.Apply(context.Background(), directory)
	require.NoError(t, err)
	require.Len(t, host.seen, 3)
	require.True(t, receipts[0].Accepted)
}

// The credential is read per request, so a projection rotated between rounds
// is the one the next round sends. This is why it is not read once at startup.
func TestARotatedProjectionIsTheOneTheNextRoundSends(t *testing.T) {
	directory := t.TempDir()
	preparedDocumentOf(t, directory, "one.json", "billing", 2, []byte("authority"))
	host := &applyHost{answers: []error{status.Error(codes.Unavailable, "not serving yet"), nil}}
	tokens := []string{"first-token", "rotated-token"}
	sender := &PairedApply{
		Client: applyHostClient(t, host),
		Credential: func() (string, error) {
			token := tokens[0]
			if len(tokens) > 1 {
				tokens = tokens[1:]
			}
			return token, nil
		},
		Sleep: noPause, Budget: time.Hour,
	}
	_, err := sender.Apply(context.Background(), directory)
	require.NoError(t, err)
	require.Len(t, host.seen, 2)
	require.Equal(t, "first-token", host.seen[0].GetCredential())
	require.Equal(t, "rotated-token", host.seen[1].GetCredential(), "the round after a rotation sends the new token")
}

// A document still unanswered when the budget is spent is a refusal, not a
// silent pass: a delivery that did not land must fail the Job that ran it.
func TestADocumentNeverAnsweredFailsTheRun(t *testing.T) {
	directory := t.TempDir()
	preparedDocumentOf(t, directory, "one.json", "billing", 2, []byte("authority"))
	host := &applyHost{answers: []error{status.Error(codes.Unavailable, "not serving yet")}}
	now := time.Now()
	clock := now
	sender := &PairedApply{
		Client: applyHostClient(t, host), Credential: func() (string, error) { return "t", nil },
		Budget: 10 * time.Second,
		Now:    func() time.Time { return clock },
		Sleep: func(_ context.Context, d time.Duration) error {
			clock = clock.Add(d + 10*time.Second)
			return nil
		},
	}
	receipts, err := sender.Apply(context.Background(), directory)
	require.ErrorContains(t, err, "one.json")
	require.ErrorContains(t, receipts[0].Err, "giving up on one.json")
	require.False(t, receipts[0].Accepted)
}

// The generation is read from the SIGNED document, and a host acknowledging a
// different one is a disagreement about what was applied, not a success.
func TestAGenerationTheHostAcknowledgesDifferentlyIsRefused(t *testing.T) {
	directory := t.TempDir()
	preparedDocumentOf(t, directory, "one.json", "billing", 4, []byte("authority"))
	sender := &PairedApply{Client: applyHostClient(t, &shiftedHost{applyHost: &applyHost{}}), Credential: func() (string, error) { return "t", nil }, Sleep: noPause}
	_, err := sender.Apply(context.Background(), directory)
	require.ErrorContains(t, err, "one.json")
}

// shiftedHost acknowledges a generation one higher than it was sent.
type shiftedHost struct{ *applyHost }

func (h *shiftedHost) ApplyManifest(ctx context.Context, request *compositionv1.ApplyManifestRequest) (*compositionv1.ApplyManifestResponse, error) {
	out, err := h.applyHost.ApplyManifest(ctx, request)
	if err != nil {
		return nil, err
	}
	out.Generation++
	return out, nil
}

// A directory with no document is not an empty success: a delivery that
// carries nothing has gone wrong upstream.
func TestADeliveryThatCarriesNothingIsRefused(t *testing.T) {
	sender := &PairedApply{Client: applyHostClient(t, &applyHost{}), Credential: func() (string, error) { return "t", nil }}
	_, err := sender.Apply(context.Background(), t.TempDir())
	require.ErrorContains(t, err, "no prepared delivery document")
}

// A projection not yet written is a refusal that names the cause, and no
// request is made with an empty credential.
func TestAnAbsentProjectionRefusesBeforeTheCall(t *testing.T) {
	directory := t.TempDir()
	preparedDocumentOf(t, directory, "one.json", "billing", 2, nil)
	host := &applyHost{}
	sender := &PairedApply{Client: applyHostClient(t, host), Credential: func() (string, error) { return "", nil }, Sleep: noPause}
	receipts, err := sender.Apply(context.Background(), directory)
	require.Error(t, err)
	require.ErrorContains(t, receipts[0].Err, "the delivery credential is empty")
	require.Empty(t, host.seen)
}

// The token never reaches a report line: the Job's log is a published record.
func TestTheCredentialIsNeverReported(t *testing.T) {
	directory := t.TempDir()
	preparedDocumentOf(t, directory, "one.json", "billing", 2, []byte("authority"))
	var log strings.Builder
	sender := &PairedApply{
		Client: applyHostClient(t, &applyHost{}), Credential: func() (string, error) { return "super-secret-token", nil },
		Sleep: noPause, Report: func(line string) { log.WriteString(line + "\n") },
	}
	_, err := sender.Apply(context.Background(), directory)
	require.NoError(t, err)
	require.Contains(t, log.String(), "delivered one.json")
	require.NotContains(t, log.String(), "super-secret-token")
}
