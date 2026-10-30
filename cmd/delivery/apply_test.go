package delivery

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/codefly-dev/cli/pkg/gitops"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// qualifyingHost is a real composition host on a real TCP listener: the command
// dials it exactly as a delivery Job dials the cell's, so what this exercises
// is the whole path — flags, dial, the credential read from a file on disk,
// the generated client and the wire.
type qualifyingHost struct {
	compositionv1.UnimplementedCompositionServiceServer
	seen []*compositionv1.ApplyManifestRequest
}

func (h *qualifyingHost) ApplyManifest(_ context.Context, request *compositionv1.ApplyManifestRequest) (*compositionv1.ApplyManifestResponse, error) {
	h.seen = append(h.seen, proto.Clone(request).(*compositionv1.ApplyManifestRequest))
	manifest, err := solutionhost.ManifestFromVerified(request.GetCarrier().GetDocument())
	if err != nil {
		return nil, err
	}
	return &compositionv1.ApplyManifestResponse{Principal: manifest.Alias, Generation: manifest.Generation}, nil
}

// The command delivers both halves to a host it dials over TCP, with the
// credential read from the projected file at execution. This is the local
// qualification of the verb a cell's delivery Job runs.
func TestApplyDeliversBothHalvesToAHostItDials(t *testing.T) {
	host := &qualifyingHost{}
	server := grpc.NewServer()
	compositionv1.RegisterCompositionServiceServer(server, host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	signer, err := solutionhost.NewInMemorySigner("dev-0123456789abcdef", ed25519.NewKeyFromSeed(make([]byte, 32)))
	require.NoError(t, err)
	manifest := &solutionhost.ModuleManifest{
		Schema: solutionhost.SchemaModuleManifestV1, Binding: "wiki", Alias: "wiki", Kind: solutionhost.KindModule, Generation: 4,
		Host:      solutionhost.HostTarget{Coordinate: "local", Domain: "local"},
		Release:   solutionhost.Release{Publisher: "test", Name: "wiki", Version: "0.1.0"},
		Workloads: []solutionhost.Workload{{Service: "api", Container: "app"}},
		Authority: &solutionhost.Authority{Principal: "wiki", InstallationRole: "installer"},
	}
	payload, err := solutionhost.SignedPayload(manifest)
	require.NoError(t, err)
	envelope, err := solutionhost.SignEnvelope(payload, signer)
	require.NoError(t, err)
	signed, err := solutionhost.Carrier(payload, envelope)
	require.NoError(t, err)
	carrier, err := solutionhost.MarshalSigned(signed)
	require.NoError(t, err)
	declaration, err := proto.Marshal(&compositionv1.AuthorityManifestV1{
		InstallationRole: "installer",
		Roles:            []*compositionv1.RoleV1{{Name: "installer"}, {Name: "editor"}, {Name: "viewer"}},
	})
	require.NoError(t, err)
	document, err := gitops.PreparedPairedDocument(carrier, declaration, signer)
	require.NoError(t, err)

	documents := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(documents, "wiki.json"), document, 0o600))
	token := filepath.Join(t.TempDir(), "delivery-token")
	require.NoError(t, os.WriteFile(token, []byte("projected-delivery-token\n"), 0o600))

	for flag, value := range map[string]string{
		"documents": documents, "address": listener.Addr().String(), "identity-file": token,
	} {
		require.NoError(t, ApplyCmd.Flags().Set(flag, value))
	}
	var output bytes.Buffer
	ApplyCmd.SetOut(&output)
	t.Cleanup(func() { ApplyCmd.SetOut(nil) })
	require.NoError(t, ApplyCmd.RunE(ApplyCmd, nil))

	require.Len(t, host.seen, 1)
	require.NotNil(t, host.seen[0].GetCarrier(), "the Core half")
	require.NotNil(t, host.seen[0].GetAuthority(), "and the authority half the legacy route cannot carry")
	require.Equal(t, "projected-delivery-token", host.seen[0].GetCredential(),
		"raw and trimmed: the projection's trailing newline is not part of the token, and the host adds the bearer scheme")
	var policy compositionv1.AuthorityManifestV1
	require.NoError(t, proto.Unmarshal(host.seen[0].GetAuthority().GetDocument(), &policy))
	require.Equal(t, []string{"installer", "editor", "viewer"},
		[]string{policy.GetRoles()[0].GetName(), policy.GetRoles()[1].GetName(), policy.GetRoles()[2].GetName()})
	require.Equal(t, "installer", policy.GetInstallationRole())
	require.Contains(t, output.String(), "delivered wiki.json")
	require.NotContains(t, output.String(), "projected-delivery-token", "a Job's log is a published record")
}

// A credential file the projection has not written yet refuses, and nothing is
// sent with an empty credential.
func TestApplyRefusesWhenTheProjectionIsAbsent(t *testing.T) {
	host := &qualifyingHost{}
	server := grpc.NewServer()
	compositionv1.RegisterCompositionServiceServer(server, host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	documents := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(documents, "one.json"), []byte(`{"carrier":{"document":"e30=","envelope":"e30="}}`), 0o600))
	for flag, value := range map[string]string{
		"documents": documents, "address": listener.Addr().String(),
		"identity-file": filepath.Join(t.TempDir(), "not-written-yet"),
	} {
		require.NoError(t, ApplyCmd.Flags().Set(flag, value))
	}
	var output bytes.Buffer
	ApplyCmd.SetOut(&output)
	t.Cleanup(func() { ApplyCmd.SetOut(nil) })
	err = ApplyCmd.RunE(ApplyCmd, nil)
	require.Error(t, err)
	require.Empty(t, host.seen)
}

// readCredential returns the token as the host takes it, with no scheme and no
// surrounding whitespace a projection's trailing newline would carry in.
func TestTheCredentialIsReadRawAndTrimmed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("  eyJhbGciOiJSUzI1NiJ9.payload.signature\n\n"), 0o600))
	credential, err := readCredential(path)
	require.NoError(t, err)
	require.Equal(t, "eyJhbGciOiJSUzI1NiJ9.payload.signature", credential)
	require.False(t, strings.HasPrefix(credential, "Bearer "), "the host adds the scheme itself")

	_, err = readCredential(filepath.Join(t.TempDir(), "absent"))
	require.ErrorContains(t, err, "read the projected delivery credential at")
}
