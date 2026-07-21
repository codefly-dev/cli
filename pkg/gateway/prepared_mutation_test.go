package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	codecore "github.com/codefly-dev/core/code"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"github.com/codefly-dev/core/policy"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPreparedMutationRequiresPinnedAuthorityAndAppliesSignedPermitOnce(t *testing.T) {
	server, privateKey, root := newPreparedMutationGateway(t)
	path := filepath.Join(root, "pkg", "service.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package service\n\nfunc Value() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	preparedResponse, err := server.PrepareMutation(t.Context(), &gatewayv1.PrepareMutationRequest{
		Service: "app", WorkspaceVersion: "workspace-v1",
		Mutation: &gatewayv1.PrepareMutationRequest_ApplyEdit{ApplyEdit: &gatewayv1.PrepareApplyEditMutation{
			File: "pkg/service.go", Find: "return 1", Replace: "return 2", FixMode: basev0.FixMode_FIX_MODE_NONE,
		}},
	})
	if err != nil || !preparedResponse.GetSuccess() {
		t.Fatalf("prepare mutation: response=%+v err=%v", preparedResponse, err)
	}
	prepared := preparedResponse.GetPrepared()
	if prepared.GetFiles()[0].ProtoReflect().Descriptor().Fields().ByName("after_content") != nil {
		t.Fatal("prepared mutation RPC exposes project bytes")
	}
	if prepared.GetExpiresAt() == nil || !prepared.GetExpiresAt().AsTime().After(time.Now().UTC()) {
		t.Fatalf("prepared mutation has no future expiry: %v", prepared.GetExpiresAt())
	}
	beforeApply, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeApply) != "package service\n\nfunc Value() int { return 1 }\n" {
		t.Fatalf("prepare wrote project bytes: %q", beforeApply)
	}
	permit := signPreparedMutationPermit(t, privateKey, prepared, time.Now().UTC().Add(-time.Second), time.Minute)

	const contenders = 2
	results := make(chan *gatewayv1.ApplyPreparedMutationResponse, contenders)
	errorsOut := make(chan error, contenders)
	var wait sync.WaitGroup
	start := make(chan struct{})
	for range contenders {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			response, applyErr := server.ApplyPreparedMutation(context.Background(), &gatewayv1.ApplyPreparedMutationRequest{
				Service: "app", PreparationId: prepared.GetPreparationId(),
				MutationDigest: prepared.GetMutationDigest(), MutationPermit: permit,
			})
			results <- response
			errorsOut <- applyErr
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsOut)
	for applyErr := range errorsOut {
		if applyErr != nil {
			t.Fatalf("apply prepared mutation RPC: %v", applyErr)
		}
	}
	var succeeded, rejected int
	for response := range results {
		if response.GetSuccess() {
			succeeded++
		} else {
			rejected++
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent single-use apply: succeeded=%d rejected=%d", succeeded, rejected)
	}
	afterApply, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterApply) != "package service\n\nfunc Value() int { return 2 }\n" {
		t.Fatalf("applied project bytes: %q", afterApply)
	}
}

func TestPreparedMutationRejectsWrongSignatureBindingExpiryAndAuthorityReplacement(t *testing.T) {
	server, privateKey, root := newPreparedMutationGateway(t)
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nconst value = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	preparedResponse, err := server.PrepareMutation(t.Context(), &gatewayv1.PrepareMutationRequest{
		Service: "app", WorkspaceVersion: "workspace-v2",
		Mutation: &gatewayv1.PrepareMutationRequest_ApplyEdit{ApplyEdit: &gatewayv1.PrepareApplyEditMutation{
			File: "main.go", Find: "value = 1", Replace: "value = 2", FixMode: basev0.FixMode_FIX_MODE_NONE,
		}},
	})
	if err != nil || !preparedResponse.GetSuccess() {
		t.Fatalf("prepare mutation: response=%+v err=%v", preparedResponse, err)
	}
	prepared := preparedResponse.GetPrepared()

	expired := signPreparedMutationPermit(t, privateKey, prepared, time.Now().UTC().Add(-2*time.Minute), time.Minute)
	response, err := server.ApplyPreparedMutation(t.Context(), &gatewayv1.ApplyPreparedMutationRequest{
		Service: "app", PreparationId: prepared.GetPreparationId(),
		MutationDigest: prepared.GetMutationDigest(), MutationPermit: expired,
	})
	if err != nil || response.GetSuccess() {
		t.Fatalf("expired permit result=%+v err=%v", response, err)
	}

	otherPublic, otherPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongSignature := signPreparedMutationPermit(t, otherPrivate, prepared, time.Now().UTC().Add(-time.Second), time.Minute)
	response, err = server.ApplyPreparedMutation(t.Context(), &gatewayv1.ApplyPreparedMutationRequest{
		Service: "app", PreparationId: prepared.GetPreparationId(),
		MutationDigest: prepared.GetMutationDigest(), MutationPermit: wrongSignature,
	})
	if err != nil || response.GetSuccess() {
		t.Fatalf("wrong signature result=%+v err=%v", response, err)
	}
	if _, err := server.ConfigureMutationAuthority(t.Context(), &gatewayv1.ConfigureMutationAuthorityRequest{
		AuthorityId: "another-authority", WorkspaceId: prepared.GetWorkspaceId(), Ed25519PublicKey: otherPublic,
	}); err == nil {
		t.Fatal("replacing pinned mutation authority succeeded")
	}
	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != "package main\n\nconst value = 1\n" {
		t.Fatalf("rejected permits changed project bytes: %q", unchanged)
	}
}

func TestPreparedMutationRetentionRejectsOversizedResults(t *testing.T) {
	server, err := NewServer(Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	after := bytes.Repeat([]byte{'x'}, maxPreparedMutationBytes+1)
	now := time.Now().UTC()
	prepared := &gatewayv1.PreparedMutation{
		SchemaVersion: preparedMutationSchemaVersion, PreparationId: uuid.NewString(),
		AuthorityId: "coordinator-key-v1", WorkspaceId: "workspace-1",
		Service: "app", WorkspaceVersion: "workspace-v1",
		Files: []*gatewayv1.PreparedFileMutation{{
			Path: "main.go", Operation: gatewayv1.PreparedFileOperation_PREPARED_FILE_OPERATION_MODIFY,
			BeforeSha256: contentSHA256([]byte("before")), AfterSha256: contentSHA256(after),
		}},
		PreparedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(preparedMutationLifetime)),
	}
	prepared.MutationDigest, err = computePreparedMutationDigest(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.storePreparedMutation(prepared, map[string][]byte{"main.go": after}); err == nil || !strings.Contains(err.Error(), "retention limit") {
		t.Fatalf("oversized prepared result error = %v, want retention limit", err)
	}
}

func newPreparedMutationGateway(t *testing.T) (*Server, ed25519.PrivateKey, string) {
	t.Helper()
	root := t.TempDir()
	server, err := NewServer(Config{WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	server.mindYAML = &MindYAML{Service: "app", Plugin: "generic"}

	codeServer := codecore.NewDefaultCodeServer(root)
	t.Cleanup(func() { _ = codeServer.Close() })
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	codev0.RegisterCodeServer(grpcServer, codeServer)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.NewClient("passthrough:///real-code-agent", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	server.plugins["app"] = &pluginConn{code: codev0.NewCodeClient(connection)}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	configured, err := server.ConfigureMutationAuthority(t.Context(), &gatewayv1.ConfigureMutationAuthorityRequest{
		AuthorityId: "coordinator-key-v1", WorkspaceId: "workspace-" + uuid.NewString(), Ed25519PublicKey: publicKey,
	})
	if err != nil || configured.GetAuthorityId() != "coordinator-key-v1" {
		t.Fatalf("configure mutation authority: response=%+v err=%v", configured, err)
	}
	return server, privateKey, root
}

func signPreparedMutationPermit(t *testing.T, privateKey ed25519.PrivateKey, prepared *gatewayv1.PreparedMutation, issuedAt time.Time, ttl time.Duration) string {
	t.Helper()
	tenantID := uuid.NewString()
	binding := mutationPermitBinding{
		AuthorityID: prepared.GetAuthorityId(), WorkspaceID: prepared.GetWorkspaceId(), Service: prepared.GetService(),
		TenantID: tenantID, PlanID: "plan-1", PlanRevision: 1,
		PlanContentHash: contentSHA256([]byte("plan-1")), LeaseSetID: uuid.NewString(), OwnerAttemptID: "attempt-1",
		WorkspaceVersion: prepared.GetWorkspaceVersion(),
		Fences:           []mutationPermitFence{{Kind: "file", Path: prepared.GetFiles()[0].GetPath(), FenceToken: 1}},
	}
	token, _, err := policy.MintEd25519(policy.MintInput{
		Principal: &policy.Principal{ID: prepared.GetAuthorityId(), Kind: policy.KindService, OrgID: tenantID},
		Action:    mutationPermitAction, Resource: prepared.GetMutationDigest(),
		AudienceID: mutationPermitAudience(prepared.GetWorkspaceId()), TTL: ttl, MaxUses: 1,
		Caveats: map[string]any{mutationPermitCaveat: binding},
		NowFunc: func() time.Time { return issuedAt },
	}, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
