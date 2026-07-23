package executionattestor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/core/executionreceipt"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	executionv1 "github.com/codefly-dev/core/generated/go/codefly/execution/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestFileAttestorPersistsStableIdentityAndSigns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "key.json")
	first, err := OpenFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Identity().SignerID != second.Identity().SignerID ||
		first.Identity().KeyID != second.Identity().KeyID {
		t.Fatalf("identity changed across reopen: first=%+v second=%+v", first.Identity(), second.Identity())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %04o", info.Mode().Perm())
	}

	attestation, err := first.Attest(testReceipt())
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Verify(attestation); err != nil {
		t.Fatal(err)
	}
}

func TestFileAttestorConcurrentCreateConverges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "key.json")
	const workers = 12
	identities := make(chan PublicIdentity, workers)
	errorsCh := make(chan error, workers)
	var start sync.WaitGroup
	start.Add(1)
	var workersWG sync.WaitGroup
	for range workers {
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			start.Wait()
			attestor, err := OpenFile(context.Background(), path)
			if err != nil {
				errorsCh <- err
				return
			}
			identities <- attestor.Identity()
		}()
	}
	start.Done()
	workersWG.Wait()
	close(identities)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}
	var expected PublicIdentity
	for identity := range identities {
		if expected.SignerID == "" {
			expected = identity
			continue
		}
		if identity.SignerID != expected.SignerID || identity.KeyID != expected.KeyID {
			t.Fatalf("concurrent creation diverged: expected=%+v got=%+v", expected, identity)
		}
	}
}

func TestFileAttestorFailsClosedOnCorruptionPermissionsAndSymlink(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(private, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte(`{"schema":"wrong"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(t.Context(), corrupt); !errors.Is(err, ErrInvalid) {
		t.Fatalf("corrupt key error = %v", err)
	}

	broad := filepath.Join(root, "broad")
	if err := os.Mkdir(broad, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(t.Context(), filepath.Join(broad, "key.json")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("broad directory error = %v", err)
	}

	target := filepath.Join(private, "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(private, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(t.Context(), link); !errors.Is(err, ErrInvalid) {
		t.Fatalf("symlink error = %v", err)
	}
}

func testReceipt() *executionv1.ExecutionReceiptV1 {
	started := time.Date(2026, time.July, 23, 19, 0, 0, 0, time.UTC)
	contextDigest := sha256.Sum256([]byte("signed-work-context"))
	return &executionv1.ExecutionReceiptV1{
		Schema:        executionreceipt.SchemaV1,
		ReceiptId:     "receipt-started",
		OperationId:   "operation-1",
		AttemptId:     "attempt-1",
		Stage:         executionv1.ExecutionStage_EXECUTION_STAGE_STARTED,
		OperationKind: "code.apply-edit",
		Producer: &executionv1.ExecutionProducerV1{
			Id: "codefly.execution", Component: "gateway", Release: "v0.1.25",
		},
		Assurance: executionv1.ExecutionAssurance_EXECUTION_ASSURANCE_GATEWAY_EXECUTED,
		WorkContext: &basev0.WorkContextV1{
			Typ: "codefly.work-context/v1", Algorithm: "Ed25519",
			KeyId: "accounts-key-1", Issuer: "accounts", Audience: "codefly.execution",
			NotBeforeUnix: started.Add(-time.Minute).Unix(), IssuedAtUnix: started.Add(-time.Minute).Unix(),
			ExpiresAtUnix: started.Add(4 * time.Minute).Unix(), Nonce: "nonce-1",
			AuthorizationRevision: 4, ReplayPolicy: "idempotent",
			TenantId: "tenant-codefly", OwnerPrincipalId: "principal-antoine",
			TaskId: "task-1", SessionId: "session-1",
		},
		WorkContextSha256: hex.EncodeToString(contextDigest[:]),
		Target:            &executionv1.ExecutionTargetV1{WorkspaceId: "workspace-codefly", Service: "warden"},
		StartedAt:         timestamppb.New(started),
	}
}
