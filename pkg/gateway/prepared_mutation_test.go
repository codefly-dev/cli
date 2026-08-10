package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"github.com/codefly-dev/core/policy"
	"github.com/google/uuid"
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
	fileIdentity := prepared.GetFiles()[0]
	if fileIdentity.GetBeforeSizeBytes() != uint64(len("package service\n\nfunc Value() int { return 1 }\n")) || fileIdentity.GetAfterSizeBytes() != uint64(len("package service\n\nfunc Value() int { return 2 }\n")) {
		t.Fatalf("prepared mutation sizes = before:%d after:%d", fileIdentity.GetBeforeSizeBytes(), fileIdentity.GetAfterSizeBytes())
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

func TestPreparedSymbolPatchRetainsBytesAndRequiresExactSymbolFence(t *testing.T) {
	server, privateKey, root := newPreparedMutationGateway(t)
	path := filepath.Join(root, "pkg", "service.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	before := "package service\n\nfunc Value() int { return 1 }\n"
	declaration := "func Value() int { return 1 }"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	preparedResponse, err := server.PrepareMutation(t.Context(), &gatewayv1.PrepareMutationRequest{
		Service: "app", WorkspaceVersion: "workspace-symbol-v1",
		Mutation: &gatewayv1.PrepareMutationRequest_SymbolPatch{SymbolPatch: &gatewayv1.PrepareSymbolPatchMutation{
			File: "pkg/service.go", SymbolId: "symbol-service-value", QualifiedName: "service.Value",
			ExpectedDeclarationSha256: contentSHA256([]byte(declaration)),
			NewSource:                 "func Value() int { return 2 }", FixMode: basev0.FixMode_FIX_MODE_NONE,
		}},
	})
	if err != nil || !preparedResponse.GetSuccess() {
		t.Fatalf("prepare symbol mutation: response=%+v err=%v", preparedResponse, err)
	}
	prepared := preparedResponse.GetPrepared()
	if len(prepared.GetFiles()) != 1 || prepared.GetFiles()[0].GetSymbolId() != "symbol-service-value" {
		t.Fatalf("prepared symbol resource = %+v", prepared.GetFiles())
	}
	if prepared.GetFiles()[0].GetBeforeSizeBytes() != uint64(len(before)) || prepared.GetFiles()[0].GetAfterSizeBytes() != uint64(len("package service\n\nfunc Value() int { return 2 }\n")) {
		t.Fatalf("prepared symbol sizes = before:%d after:%d", prepared.GetFiles()[0].GetBeforeSizeBytes(), prepared.GetFiles()[0].GetAfterSizeBytes())
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != before {
		t.Fatalf("preparation changed source: content=%q err=%v", unchanged, err)
	}
	wrongFencePermit := signPreparedMutationPermitWithFence(t, privateKey, prepared, mutationPermitFence{
		Kind: "file", Path: "pkg/service.go", FenceToken: 1,
	}, time.Now().UTC().Add(-time.Second), time.Minute)
	rejected, err := server.ApplyPreparedMutation(t.Context(), &gatewayv1.ApplyPreparedMutationRequest{
		Service: "app", PreparationId: prepared.GetPreparationId(),
		MutationDigest: prepared.GetMutationDigest(), MutationPermit: wrongFencePermit,
	})
	if err != nil || rejected.GetSuccess() || !strings.Contains(rejected.GetError(), "no exact fence") {
		t.Fatalf("file fence authorized symbol mutation: response=%+v err=%v", rejected, err)
	}
	permit := signPreparedMutationPermit(t, privateKey, prepared, time.Now().UTC().Add(-time.Second), time.Minute)
	applied, err := server.ApplyPreparedMutation(t.Context(), &gatewayv1.ApplyPreparedMutationRequest{
		Service: "app", PreparationId: prepared.GetPreparationId(),
		MutationDigest: prepared.GetMutationDigest(), MutationPermit: permit,
	})
	if err != nil || !applied.GetSuccess() {
		t.Fatalf("apply prepared symbol mutation: response=%+v err=%v", applied, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != "package service\n\nfunc Value() int { return 2 }\n" {
		t.Fatalf("applied source=%q err=%v", after, err)
	}
}

func TestSymbolPatchRecoveryReasonSurvivesGatewayAndPreparation(t *testing.T) {
	server, _, root := newPreparedMutationGateway(t)
	path := filepath.Join(root, "pkg", "service.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	declaration := "func Value() int { return 1 }"
	if err := os.WriteFile(path, []byte("package service\n\n"+declaration+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	request := &gatewayv1.ApplySymbolPatchRequest{
		Service: "app", File: "pkg/service.go", QualifiedName: "service.Value",
		ExpectedDeclarationSha256: strings.Repeat("0", 64),
		NewSource:                 "func Value() int { return 2 }", DryRun: true,
	}
	direct, err := server.ApplySymbolPatch(t.Context(), request)
	if err != nil || direct.GetSuccess() || direct.GetFailureReason() != basev0.SymbolPatchFailureReason_SYMBOL_PATCH_FAILURE_REASON_STALE_ANCHOR {
		t.Fatalf("direct stale response=%+v err=%v", direct, err)
	}
	prepared, err := server.PrepareMutation(t.Context(), &gatewayv1.PrepareMutationRequest{
		Service: "app", WorkspaceVersion: "workspace-stale-v1",
		Mutation: &gatewayv1.PrepareMutationRequest_SymbolPatch{SymbolPatch: &gatewayv1.PrepareSymbolPatchMutation{
			File: request.GetFile(), SymbolId: "symbol-service-value", QualifiedName: request.GetQualifiedName(),
			ExpectedDeclarationSha256: request.GetExpectedDeclarationSha256(), NewSource: request.GetNewSource(),
		}},
	})
	if err != nil || prepared.GetSuccess() || prepared.GetSymbolPatchFailureReason() != basev0.SymbolPatchFailureReason_SYMBOL_PATCH_FAILURE_REASON_STALE_ANCHOR {
		t.Fatalf("prepared stale response=%+v err=%v", prepared, err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != "package service\n\n"+declaration+"\n" {
		t.Fatalf("stale attempts changed project bytes: content=%q err=%v", unchanged, err)
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
			BeforeSizeBytes: uint64(len("before")), AfterSizeBytes: uint64(len(after)),
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
	t.Cleanup(func() { _ = server.Close() })

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
	fence := mutationPermitFence{Kind: "file", Path: prepared.GetFiles()[0].GetPath(), FenceToken: 1}
	if symbolID := prepared.GetFiles()[0].GetSymbolId(); symbolID != "" {
		fence.Kind = "symbol"
		fence.SymbolID = symbolID
	}
	return signPreparedMutationPermitWithFence(t, privateKey, prepared, fence, issuedAt, ttl)
}

func signPreparedMutationPermitWithFence(t *testing.T, privateKey ed25519.PrivateKey, prepared *gatewayv1.PreparedMutation, fence mutationPermitFence, issuedAt time.Time, ttl time.Duration) string {
	t.Helper()
	tenantID := uuid.NewString()
	binding := mutationPermitBinding{
		AuthorityID: prepared.GetAuthorityId(), WorkspaceID: prepared.GetWorkspaceId(), Service: prepared.GetService(),
		TenantID: tenantID, PlanID: "plan-1", PlanRevision: 1,
		PlanContentHash: contentSHA256([]byte("plan-1")), LeaseSetID: uuid.NewString(), OwnerAttemptID: "attempt-1",
		WorkspaceVersion: prepared.GetWorkspaceVersion(),
		Fences:           []mutationPermitFence{fence},
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
