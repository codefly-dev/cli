package executionrecorder

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/executionattestor"
	"github.com/codefly-dev/cli/pkg/executionjournal"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	executionv1 "github.com/codefly-dev/core/generated/go/codefly/execution/v1"
	codefly "github.com/codefly-dev/sdk-go"
	"google.golang.org/protobuf/proto"
)

func TestRecorderBracketsEffectAndPreventsOperationReplay(t *testing.T) {
	fixture := newRecorderFixture(t)
	input := fixture.beginInput()

	first, err := fixture.recorder.Begin(t.Context(), fixture.execution, input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Attempt == nil || first.Existing != nil {
		t.Fatalf("first begin = %+v", first)
	}
	incomplete, err := fixture.journal.IncompleteAttempts(t.Context(), 10)
	if err != nil || len(incomplete) != 1 {
		t.Fatalf("incomplete after begin = %+v err=%v", incomplete, err)
	}

	retry, err := fixture.recorder.Begin(t.Context(), fixture.execution, input)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Attempt != nil || retry.Existing == nil ||
		retry.Existing.Attestation.GetReceipt().GetStage() != executionv1.ExecutionStage_EXECUTION_STAGE_STARTED {
		t.Fatalf("retry begin = %+v", retry)
	}

	after := proto.Clone(input.Resources[0]).(*executionv1.ExecutionResourceV1)
	after.Changed = true
	afterDigest := sha256.Sum256([]byte("after"))
	after.AfterSha256 = stringPointer(hexDigest(afterDigest))
	terminal, err := first.Attempt.Finish(t.Context(), FinishInput{
		Stage:     executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED,
		Resources: []*executionv1.ExecutionResourceV1{after},
		Result:    &executionv1.ExecutionResultV1{Status: "passed", DurationMs: 125, PassedCount: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.GetReceipt().GetStage() != executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED {
		t.Fatalf("terminal stage = %s", terminal.GetReceipt().GetStage())
	}
	if _, err := first.Attempt.Finish(t.Context(), FinishInput{
		Stage:  executionv1.ExecutionStage_EXECUTION_STAGE_FAILED,
		Result: &executionv1.ExecutionResultV1{Status: "failed"},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second finish error = %v", err)
	}

	completedRetry, err := fixture.recorder.Begin(t.Context(), fixture.execution, input)
	if err != nil {
		t.Fatal(err)
	}
	if completedRetry.Existing == nil ||
		completedRetry.Existing.Attestation.GetReceipt().GetStage() != executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED {
		t.Fatalf("completed retry = %+v", completedRetry)
	}

	substituted := input
	substituted.Target = proto.Clone(input.Target).(*executionv1.ExecutionTargetV1)
	substituted.Target.Service = "another-service"
	if _, err := fixture.recorder.Begin(t.Context(), fixture.execution, substituted); !errors.Is(err, ErrConflict) {
		t.Fatalf("target substitution error = %v", err)
	}

	substituted = input
	substituted.OperationInputSHA256 = hexDigest(sha256.Sum256([]byte("different input")))
	if _, err := fixture.recorder.Begin(t.Context(), fixture.execution, substituted); !errors.Is(err, ErrConflict) {
		t.Fatalf("operation input substitution error = %v", err)
	}

	// Workspace observations can legitimately change after the effect. Replay
	// identity remains the stable operation input, not the mutable before hash.
	changedObservation := input
	changedObservation.Resources = cloneResources(input.Resources)
	changedObservation.Resources[0].BeforeSha256 = stringPointer(hexDigest(sha256.Sum256([]byte("after"))))
	replayed, err := fixture.recorder.Begin(t.Context(), fixture.execution, changedObservation)
	if err != nil || replayed.Existing == nil {
		t.Fatalf("replay after workspace mutation = %+v err=%v", replayed, err)
	}
}

func TestRecorderRecoversIncompleteStartAsUncertain(t *testing.T) {
	fixture := newRecorderFixture(t)
	input := fixture.beginInput()
	if result, err := fixture.recorder.Begin(t.Context(), fixture.execution, input); err != nil || result.Attempt == nil {
		t.Fatalf("begin = %+v err=%v", result, err)
	}
	if err := fixture.journal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := executionjournal.Open(t.Context(), fixture.journalPath, fixture.attestor.Verify)
	if err != nil {
		t.Fatal(err)
	}
	fixture.journal = reopened
	t.Cleanup(func() { _ = reopened.Close() })
	recorder, err := New(Config{
		Journal: reopened, Attestor: fixture.attestor, Authority: fixture.authority,
		Producer: fixture.producer, Now: fixture.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recorder.RecoverIncomplete(t.Context(), 10)
	if err != nil || recovered != 1 {
		t.Fatalf("recovered = %d err=%v", recovered, err)
	}
	incomplete, err := reopened.IncompleteAttempts(t.Context(), 10)
	if err != nil || len(incomplete) != 0 {
		t.Fatalf("incomplete after recovery = %+v err=%v", incomplete, err)
	}
	retry, err := recorder.Begin(t.Context(), fixture.execution, input)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Existing == nil ||
		retry.Existing.Attestation.GetReceipt().GetStage() != executionv1.ExecutionStage_EXECUTION_STAGE_UNCERTAIN {
		t.Fatalf("retry after recovery = %+v", retry)
	}
}

func TestRecorderRejectsAuthorityFailureBeforeJournal(t *testing.T) {
	fixture := newRecorderFixture(t)
	recorder, err := New(Config{
		Journal:  fixture.journal,
		Attestor: fixture.attestor,
		Authority: AuthorityFunc(func(
			context.Context,
			codefly.WorkContextToken,
			Admission,
		) (*basev0.WorkContextV1, error) {
			return nil, errors.New("forged")
		}),
		Producer: fixture.producer,
		Now:      fixture.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Begin(t.Context(), fixture.execution, fixture.beginInput()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("authority error = %v", err)
	}
	pending, err := fixture.journal.Pending(t.Context(), 0, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after rejected authority = %+v err=%v", pending, err)
	}
}

func TestRecorderRejectsTargetOutsideWorkContextBeforeJournal(t *testing.T) {
	fixture := newRecorderFixture(t)
	for _, mutate := range []func(*BeginInput){
		func(input *BeginInput) { input.Target.WorkspaceId = "workspace-other" },
		func(input *BeginInput) {
			projectID := "project-other"
			input.Target.ProjectId = &projectID
		},
	} {
		input := fixture.beginInput()
		mutate(&input)
		if _, err := fixture.recorder.Begin(t.Context(), fixture.execution, input); !errors.Is(err, ErrInvalid) {
			t.Fatalf("target mismatch error = %v", err)
		}
	}
	pending, err := fixture.journal.Pending(t.Context(), 0, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after target mismatch = %+v err=%v", pending, err)
	}
}

type recorderFixture struct {
	recorder    *Recorder
	journal     *executionjournal.Journal
	journalPath string
	attestor    *executionattestor.FileAttestor
	authority   Authority
	producer    *executionv1.ExecutionProducerV1
	execution   codefly.ExecutionContext
	clock       *testClock
}

func newRecorderFixture(t *testing.T) *recorderFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "execution")
	attestor, err := executionattestor.OpenFile(t.Context(), filepath.Join(root, "key", "gateway.json"))
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "journal", "receipts.db")
	journal, err := executionjournal.Open(t.Context(), journalPath, attestor.Verify)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })

	signature := make([]byte, 64)
	token, err := codefly.ParseWorkContextToken("e30." + base64.RawURLEncoding.EncodeToString(signature))
	if err != nil {
		t.Fatal(err)
	}
	execution, err := codefly.NewExecutionContext(token, "operation-1")
	if err != nil {
		t.Fatal(err)
	}
	claims := testClaims()
	authority := AuthorityFunc(func(
		_ context.Context,
		_ codefly.WorkContextToken,
		admission Admission,
	) (*basev0.WorkContextV1, error) {
		if admission.OperationID != "operation-1" || admission.ProducerID != "codefly.execution" {
			return nil, errors.New("unexpected admission")
		}
		return proto.Clone(claims).(*basev0.WorkContextV1), nil
	})
	producer := &executionv1.ExecutionProducerV1{
		Id: "codefly.execution", Component: "gateway", Release: "v0.1.25",
	}
	clock := &testClock{next: time.Date(2026, time.July, 23, 19, 0, 0, 0, time.UTC)}
	recorder, err := New(Config{
		Journal: journal, Attestor: attestor, Authority: authority, Producer: producer, Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &recorderFixture{
		recorder: recorder, journal: journal, journalPath: journalPath,
		attestor: attestor, authority: authority, producer: producer,
		execution: execution, clock: clock,
	}
}

func (f *recorderFixture) beginInput() BeginInput {
	before := sha256.Sum256([]byte("before"))
	projectID := "project-warden"
	return BeginInput{
		OperationKind:        "code.apply-edit",
		OperationInputSHA256: hexDigest(sha256.Sum256([]byte("apply-edit request"))),
		Assurance:            executionv1.ExecutionAssurance_EXECUTION_ASSURANCE_PLUGIN_EXECUTED,
		Target: &executionv1.ExecutionTargetV1{
			WorkspaceId: "workspace-codefly", Service: "warden", ProjectId: &projectID,
		},
		Resources: []*executionv1.ExecutionResourceV1{{
			Kind: "workspace.path", Reference: "modules/warden/main.go",
			BeforeSha256: stringPointer(hexDigest(before)),
		}},
	}
}

func testClaims() *basev0.WorkContextV1 {
	started := time.Date(2026, time.July, 23, 19, 0, 0, 0, time.UTC)
	workspaceID := "workspace-codefly"
	projectID := "project-warden"
	return &basev0.WorkContextV1{
		Typ: "codefly.work-context/v1", Algorithm: "Ed25519",
		KeyId: "accounts-key-1", Issuer: "accounts", Audience: "codefly.execution",
		NotBeforeUnix: started.Add(-time.Minute).Unix(), IssuedAtUnix: started.Add(-time.Minute).Unix(),
		ExpiresAtUnix: started.Add(4 * time.Minute).Unix(), Nonce: "nonce-1",
		AuthorizationRevision: 4, ReplayPolicy: "idempotent",
		TenantId: "tenant-codefly", OwnerPrincipalId: "principal-antoine",
		TaskId: "task-1", SessionId: "session-1",
		WorkspaceId: &workspaceID, ProjectId: &projectID,
	}
}

type testClock struct {
	mu   sync.Mutex
	next time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	value := c.next
	c.next = c.next.Add(125 * time.Millisecond)
	return value
}

func hexDigest(sum [sha256.Size]byte) string {
	const digits = "0123456789abcdef"
	encoded := make([]byte, len(sum)*2)
	for index, value := range sum {
		encoded[index*2] = digits[value>>4]
		encoded[index*2+1] = digits[value&0x0f]
	}
	return string(encoded)
}
