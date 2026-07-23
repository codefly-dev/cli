package executionjournal

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/core/executionreceipt"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	executionv1 "github.com/codefly-dev/core/generated/go/codefly/execution/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestJournalAppendReplayAckRestartAndPrune(t *testing.T) {
	journal, path, privateKey := newTestJournal(t)
	started := attest(t, privateKey, receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_STARTED))
	startedResult, err := journal.Append(t.Context(), started)
	if err != nil {
		t.Fatal(err)
	}
	if startedResult.Sequence != 1 || startedResult.Duplicate {
		t.Fatalf("started result = %+v", startedResult)
	}
	lookedUp, found, err := journal.Lookup(t.Context(), started.GetReceipt().GetReceiptId())
	if err != nil || !found || lookedUp.Sequence != startedResult.Sequence ||
		lookedUp.Attestation.GetReceipt().GetPayloadSha256() != started.GetReceipt().GetPayloadSha256() {
		t.Fatalf("lookup = %+v found=%t err=%v", lookedUp, found, err)
	}
	if _, found, err := journal.Lookup(t.Context(), "missing-receipt"); err != nil || found {
		t.Fatalf("missing lookup found=%t err=%v", found, err)
	}
	duplicate, err := journal.Append(t.Context(), started)
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.Duplicate || duplicate.Sequence != startedResult.Sequence {
		t.Fatalf("duplicate result = %+v", duplicate)
	}

	succeeded := attest(t, privateKey, receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED))
	terminalResult, err := journal.Append(t.Context(), succeeded)
	if err != nil {
		t.Fatal(err)
	}
	if terminalResult.Sequence != 2 {
		t.Fatalf("terminal sequence = %d", terminalResult.Sequence)
	}
	pending, err := journal.Pending(t.Context(), 0, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %v err=%v", pending, err)
	}
	ack, err := journal.Acknowledge(t.Context(), started.GetReceipt().GetReceiptId(), started.GetReceipt().GetPayloadSha256())
	if err != nil || ack.Duplicate {
		t.Fatalf("first ack = %+v err=%v", ack, err)
	}
	ack, err = journal.Acknowledge(t.Context(), started.GetReceipt().GetReceiptId(), started.GetReceipt().GetPayloadSha256())
	if err != nil || !ack.Duplicate {
		t.Fatalf("duplicate ack = %+v err=%v", ack, err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	journal = reopenTestJournal(t, path, privateKey)
	t.Cleanup(func() { _ = journal.Close() })
	pending, err = journal.Pending(t.Context(), 0, 10)
	if err != nil || len(pending) != 1 || pending[0].Sequence != 2 {
		t.Fatalf("restart pending = %+v err=%v", pending, err)
	}
	if pruned, err := journal.PruneAcknowledgedThrough(t.Context(), 2); err != nil || pruned != 0 {
		t.Fatalf("partial attempt prune = %d err=%v", pruned, err)
	}
	if _, err := journal.Acknowledge(t.Context(), succeeded.GetReceipt().GetReceiptId(), succeeded.GetReceipt().GetPayloadSha256()); err != nil {
		t.Fatal(err)
	}
	if pruned, err := journal.PruneAcknowledgedThrough(t.Context(), 2); err != nil || pruned != 2 {
		t.Fatalf("complete attempt prune = %d err=%v", pruned, err)
	}
	pending, err = journal.Pending(t.Context(), 0, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after prune = %+v err=%v", pending, err)
	}
}

func TestJournalRejectsTerminalWithoutStartAndAttemptSubstitution(t *testing.T) {
	journal, _, privateKey := newTestJournal(t)
	t.Cleanup(func() { _ = journal.Close() })

	terminal := attest(t, privateKey, receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED))
	if _, err := journal.Append(t.Context(), terminal); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal without start error = %v", err)
	}
	started := attest(t, privateKey, receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_STARTED))
	if _, err := journal.Append(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	substituted := receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED)
	substituted.Target.Service = "another-service"
	if _, err := journal.Append(t.Context(), attest(t, privateKey, substituted)); !errors.Is(err, ErrConflict) {
		t.Fatalf("attempt substitution error = %v", err)
	}
	conflictingID := proto.Clone(started).(*executionv1.ExecutionAttestationV1)
	conflictingID.Receipt.OperationKind = "test.run"
	conflictingID.Receipt.PayloadSha256 = ""
	conflictingID = attest(t, privateKey, conflictingID.Receipt)
	if _, err := journal.Append(t.Context(), conflictingID); !errors.Is(err, ErrConflict) {
		t.Fatalf("receipt ID conflict error = %v", err)
	}
}

func TestJournalRejectsReopeningOrReterminatingAttempt(t *testing.T) {
	journal, _, privateKey := newTestJournal(t)
	t.Cleanup(func() { _ = journal.Close() })

	started := attest(t, privateKey, receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_STARTED))
	if _, err := journal.Append(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	succeeded := attest(t, privateKey, receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED))
	if _, err := journal.Append(t.Context(), succeeded); err != nil {
		t.Fatal(err)
	}
	admitted := attest(t, privateKey, receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_ADMITTED))
	if _, err := journal.Append(t.Context(), admitted); !errors.Is(err, ErrConflict) {
		t.Fatalf("admitted after terminal error = %v", err)
	}
	failed := attest(t, privateKey, receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_FAILED))
	if _, err := journal.Append(t.Context(), failed); !errors.Is(err, ErrConflict) {
		t.Fatalf("second terminal error = %v", err)
	}
}

func TestJournalPendingCursorCannotSkipUnacknowledgedGap(t *testing.T) {
	journal, _, privateKey := newTestJournal(t)
	t.Cleanup(func() { _ = journal.Close() })

	first := attest(t, privateKey, receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_STARTED))
	if _, err := journal.Append(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	secondReceipt := receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_STARTED)
	secondReceipt.ReceiptId = "receipt-started-2"
	secondReceipt.OperationId = "operation-2"
	secondReceipt.AttemptId = "attempt-2"
	second := attest(t, privateKey, secondReceipt)
	if _, err := journal.Append(t.Context(), second); err != nil {
		t.Fatal(err)
	}

	pending, err := journal.Pending(t.Context(), 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].Sequence != 1 || pending[1].Sequence != 2 {
		t.Fatalf("cursor skipped unacknowledged gap: %+v", pending)
	}
	if _, err := journal.Acknowledge(
		t.Context(),
		first.GetReceipt().GetReceiptId(),
		first.GetReceipt().GetPayloadSha256(),
	); err != nil {
		t.Fatal(err)
	}
	pending, err = journal.Pending(t.Context(), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Sequence != 2 {
		t.Fatalf("cursor did not resume after acknowledged prefix: %+v", pending)
	}
}

func TestJournalIsolatesAcknowledgementsPerExporterPlugin(t *testing.T) {
	journal, _, privateKey := newTestJournal(t)
	t.Cleanup(func() { _ = journal.Close() })

	started := attest(t, privateKey, receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_STARTED))
	succeeded := attest(t, privateKey, receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED))
	if _, err := journal.Append(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(t.Context(), succeeded); err != nil {
		t.Fatal(err)
	}

	for _, exporterID := range []string{"warden.execution", "audit.archive"} {
		pending, err := journal.PendingFor(t.Context(), exporterID, 0, 10)
		if err != nil || len(pending) != 2 {
			t.Fatalf("%s initial pending=%d err=%v", exporterID, len(pending), err)
		}
	}
	if _, err := journal.AcknowledgeFor(
		t.Context(),
		"warden.execution",
		started.GetReceipt().GetReceiptId(),
		started.GetReceipt().GetPayloadSha256(),
	); err != nil {
		t.Fatal(err)
	}
	wardenPending, err := journal.PendingFor(t.Context(), "warden.execution", 0, 10)
	if err != nil || len(wardenPending) != 1 || wardenPending[0].Sequence != 2 {
		t.Fatalf("warden pending=%+v err=%v", wardenPending, err)
	}
	auditPending, err := journal.PendingFor(t.Context(), "audit.archive", 0, 10)
	if err != nil || len(auditPending) != 2 {
		t.Fatalf("audit pending=%+v err=%v", auditPending, err)
	}
	if pruned, err := journal.PruneAcknowledgedThroughFor(
		t.Context(),
		2,
		[]string{"warden.execution", "audit.archive"},
	); err != nil || pruned != 0 {
		t.Fatalf("pruned before every exporter ack=%d err=%v", pruned, err)
	}

	for _, exporterID := range []string{"warden.execution", "audit.archive"} {
		for _, attestation := range []*executionv1.ExecutionAttestationV1{started, succeeded} {
			if _, err := journal.AcknowledgeFor(
				t.Context(),
				exporterID,
				attestation.GetReceipt().GetReceiptId(),
				attestation.GetReceipt().GetPayloadSha256(),
			); err != nil {
				t.Fatal(err)
			}
		}
	}
	if pruned, err := journal.PruneAcknowledgedThroughFor(
		t.Context(),
		2,
		[]string{"warden.execution", "audit.archive"},
	); err != nil || pruned != 2 {
		t.Fatalf("pruned after all exporter acks=%d err=%v", pruned, err)
	}
}

func TestJournalReportsIncompleteAttemptAfterRestart(t *testing.T) {
	journal, path, privateKey := newTestJournal(t)
	started := attest(t, privateKey, receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_STARTED))
	if _, err := journal.Append(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	journal = reopenTestJournal(t, path, privateKey)
	t.Cleanup(func() { _ = journal.Close() })
	incomplete, err := journal.IncompleteAttempts(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(incomplete) != 1 || incomplete[0].Attestation.GetReceipt().GetReceiptId() != "receipt-started" {
		t.Fatalf("incomplete = %+v", incomplete)
	}
}

func TestJournalFailsClosedOnUnsafePermissionsSymlinkAndLock(t *testing.T) {
	root := t.TempDir()
	privateDir := filepath.Join(root, "private")
	if err := os.Mkdir(privateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	privateKey := testPrivateKey(t)
	if _, err := Open(t.Context(), filepath.Join(privateDir, "journal.db"), testVerifier(privateKey)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("broad directory error = %v", err)
	}

	privateDir = filepath.Join(root, "owner-only")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(privateDir, "target.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(privateDir, "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), link, testVerifier(privateKey)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("symlink error = %v", err)
	}

	path := filepath.Join(privateDir, "locked.db")
	first, err := Open(t.Context(), path, testVerifier(privateKey))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := Open(ctx, path, testVerifier(privateKey)); err == nil {
		t.Fatal("second process-style open unexpectedly acquired the lock")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatal("journal lock failure exceeded bounded open timeout")
	}
}

func TestJournalRejectsTruncatedDatabase(t *testing.T) {
	journal, path, privateKey := newTestJournal(t)
	started := attest(t, privateKey, receiptForStage(executionv1.ExecutionStage_EXECUTION_STAGE_STARTED))
	if _, err := journal.Append(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, info.Size()-1); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), path, testVerifier(privateKey)); err == nil {
		t.Fatal("truncated journal opened successfully")
	}
}

func newTestJournal(t *testing.T) (*Journal, string, ed25519.PrivateKey) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "journal")
	path := filepath.Join(root, "execution.db")
	privateKey := testPrivateKey(t)
	journal, err := Open(t.Context(), path, testVerifier(privateKey))
	if err != nil {
		t.Fatal(err)
	}
	return journal, path, privateKey
}

func reopenTestJournal(t *testing.T, path string, privateKey ed25519.PrivateKey) *Journal {
	t.Helper()
	journal, err := Open(t.Context(), path, testVerifier(privateKey))
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func testPrivateKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey
}

func testVerifier(privateKey ed25519.PrivateKey) Verifier {
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return func(attestation *executionv1.ExecutionAttestationV1) error {
		_, err := executionreceipt.Verify(attestation, publicKey)
		return err
	}
}

func attest(t *testing.T, privateKey ed25519.PrivateKey, receipt *executionv1.ExecutionReceiptV1) *executionv1.ExecutionAttestationV1 {
	t.Helper()
	attestation, err := executionreceipt.Attest(receipt, "gateway-installation-1", "gateway-key-1", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return attestation
}

func receiptForStage(stage executionv1.ExecutionStage) *executionv1.ExecutionReceiptV1 {
	started := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	workspaceID := "workspace-codefly"
	projectID := "project-warden"
	parentSessionID := "session-root"
	contextDigest := sha256.Sum256([]byte("signed-work-context"))
	before := sha256.Sum256([]byte("before"))
	receipt := &executionv1.ExecutionReceiptV1{
		Schema:        executionreceipt.SchemaV1,
		ReceiptId:     "receipt-" + stageName(stage),
		OperationId:   "operation-1",
		AttemptId:     "attempt-1",
		Stage:         stage,
		OperationKind: "code.apply-edit",
		Producer: &executionv1.ExecutionProducerV1{
			Id: "codefly.execution", Component: "gateway", Release: "v0.1.24",
		},
		Assurance: executionv1.ExecutionAssurance_EXECUTION_ASSURANCE_PLUGIN_EXECUTED,
		WorkContext: &basev0.WorkContextV1{
			Typ: "codefly.work-context/v1", Algorithm: "Ed25519",
			KeyId: "accounts-key-1", Issuer: "accounts", Audience: "codefly.execution",
			NotBeforeUnix: started.Add(-time.Minute).Unix(), IssuedAtUnix: started.Add(-time.Minute).Unix(),
			ExpiresAtUnix: started.Add(4 * time.Minute).Unix(), Nonce: "nonce-1",
			AuthorizationRevision: 4, ReplayPolicy: "idempotent",
			TenantId: "tenant-codefly", OwnerPrincipalId: "principal-antoine",
			TaskId: "task-1", SessionId: "session-child", ParentSessionId: &parentSessionID,
			AuthorityScopes: []*basev0.WorkScopeV1{{
				ResourceKind: "evidence", Actions: []string{"append"}, ResourceIds: []string{"codefly.execution"},
			}},
			ActorChain: []*basev0.WorkActorV1{{
				PrincipalId: "principal-claude", PrincipalKind: "agent", DelegationId: "delegation-1",
				GrantedScopes: []*basev0.WorkScopeV1{{
					ResourceKind: "evidence", Actions: []string{"append"}, ResourceIds: []string{"codefly.execution"},
				}},
			}},
			WorkspaceId: &workspaceID, ProjectId: &projectID,
		},
		WorkContextSha256: hex.EncodeToString(contextDigest[:]),
		Target:            &executionv1.ExecutionTargetV1{WorkspaceId: workspaceID, Service: "warden", ProjectId: &projectID},
		StartedAt:         timestamppb.New(started),
		Resources: []*executionv1.ExecutionResourceV1{{
			Kind: "workspace.path", Reference: "modules/warden/main.go",
			BeforeSha256: stringPointer(hex.EncodeToString(before[:])),
		}},
	}
	if terminal(stage) {
		completed := started.Add(725 * time.Millisecond)
		receipt.CompletedAt = timestamppb.New(completed)
		receipt.Result = &executionv1.ExecutionResultV1{Status: "passed", DurationMs: 725, PassedCount: 1}
	}
	return receipt
}

func stageName(stage executionv1.ExecutionStage) string {
	switch stage {
	case executionv1.ExecutionStage_EXECUTION_STAGE_ADMITTED:
		return "admitted"
	case executionv1.ExecutionStage_EXECUTION_STAGE_STARTED:
		return "started"
	case executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED:
		return "succeeded"
	case executionv1.ExecutionStage_EXECUTION_STAGE_FAILED:
		return "failed"
	case executionv1.ExecutionStage_EXECUTION_STAGE_UNCERTAIN:
		return "uncertain"
	default:
		return "other"
	}
}

func stringPointer(value string) *string {
	return &value
}
