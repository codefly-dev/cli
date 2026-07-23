// Package executionrecorder implements the product-neutral Gateway lifecycle
// boundary around effects. Authority verification and attestation custody are
// injected capabilities; exporters and products are not part of this package.
package executionrecorder

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/codefly-dev/cli/pkg/executionattestor"
	"github.com/codefly-dev/cli/pkg/executionjournal"
	"github.com/codefly-dev/core/executionreceipt"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	executionv1 "github.com/codefly-dev/core/generated/go/codefly/execution/v1"
	codefly "github.com/codefly-dev/sdk-go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	// ErrInvalid is returned for malformed recorder configuration or input.
	ErrInvalid = errors.New("invalid Codefly execution recorder input")
	// ErrConflict means one logical operation was reused with different
	// authority, target, producer, or operation facts.
	ErrConflict = errors.New("Codefly execution recorder conflict")
)

// Admission describes the effect boundary an authority is asked to verify.
// Implementations can use Accounts, a local key registry, or another
// product-neutral authority without changing the recorder.
type Admission struct {
	OperationID   string
	OperationKind string
	ProducerID    string
	Assurance     executionv1.ExecutionAssurance
	Target        *executionv1.ExecutionTargetV1
}

// Authority verifies the opaque SDK token and returns trusted claims.
type Authority interface {
	Verify(context.Context, codefly.WorkContextToken, Admission) (*basev0.WorkContextV1, error)
}

// AuthorityFunc adapts a function into Authority.
type AuthorityFunc func(context.Context, codefly.WorkContextToken, Admission) (*basev0.WorkContextV1, error)

// Verify implements Authority.
func (fn AuthorityFunc) Verify(
	ctx context.Context,
	token codefly.WorkContextToken,
	admission Admission,
) (*basev0.WorkContextV1, error) {
	return fn(ctx, token, admission)
}

// Journal is the minimal durable outbox contract used by the recorder.
type Journal interface {
	Append(context.Context, *executionv1.ExecutionAttestationV1) (executionjournal.AppendResult, error)
	Lookup(context.Context, string) (executionjournal.Entry, bool, error)
	IncompleteAttempts(context.Context, int) ([]executionjournal.Entry, error)
}

// Config freezes the neutral producer and injected trust capabilities.
type Config struct {
	Journal   Journal
	Attestor  executionattestor.Attestor
	Authority Authority
	Producer  *executionv1.ExecutionProducerV1
	Now       func() time.Time
}

// Recorder durably brackets one real effect with signed lifecycle facts.
type Recorder struct {
	journal   Journal
	attestor  executionattestor.Attestor
	authority Authority
	producer  *executionv1.ExecutionProducerV1
	now       func() time.Time
}

// BeginInput contains only bounded, product-neutral facts known before an
// effect starts.
type BeginInput struct {
	OperationKind        string
	OperationInputSHA256 string
	Assurance            executionv1.ExecutionAssurance
	Target               *executionv1.ExecutionTargetV1
	Resources            []*executionv1.ExecutionResourceV1
}

// BeginResult tells the Gateway whether it owns a new effect attempt. Existing
// is returned for an operation already started or terminated; the caller must
// not execute the effect again.
type BeginResult struct {
	Attempt  *Attempt
	Existing *executionjournal.Entry
}

// Attempt is the only capability that can append a terminal fact for a start.
type Attempt struct {
	mu       sync.Mutex
	recorder *Recorder
	started  *executionv1.ExecutionReceiptV1
	finished bool
}

// FinishInput contains bounded facts observed after the effect.
type FinishInput struct {
	Stage     executionv1.ExecutionStage
	Resources []*executionv1.ExecutionResourceV1
	Result    *executionv1.ExecutionResultV1
}

// New validates and freezes one recorder.
func New(config Config) (*Recorder, error) {
	if config.Journal == nil || config.Attestor == nil || config.Authority == nil {
		return nil, fmt.Errorf("%w: journal, attestor, and authority are required", ErrInvalid)
	}
	if config.Producer == nil {
		return nil, fmt.Errorf("%w: producer is required", ErrInvalid)
	}
	producer := proto.Clone(config.Producer).(*executionv1.ExecutionProducerV1)
	if producer.GetId() == "" || producer.GetComponent() == "" || producer.GetRelease() == "" {
		return nil, fmt.Errorf("%w: producer ID, component, and release are required", ErrInvalid)
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Recorder{
		journal:   config.Journal,
		attestor:  config.Attestor,
		authority: config.Authority,
		producer:  producer,
		now:       now,
	}, nil
}

// Begin verifies the SDK carrier, detects operation replay before the effect,
// and durably appends STARTED. A non-nil Existing means the caller must not
// execute the effect again.
func (r *Recorder) Begin(
	ctx context.Context,
	execution codefly.ExecutionContext,
	input BeginInput,
) (BeginResult, error) {
	if r == nil {
		return BeginResult{}, fmt.Errorf("%w: recorder is nil", ErrInvalid)
	}
	if ctx == nil {
		return BeginResult{}, fmt.Errorf("%w: context is required", ErrInvalid)
	}
	if input.Target == nil {
		return BeginResult{}, fmt.Errorf("%w: target is required", ErrInvalid)
	}
	if strings.TrimSpace(input.OperationKind) == "" || len(input.OperationKind) > 128 {
		return BeginResult{}, fmt.Errorf("%w: bounded operation kind is required", ErrInvalid)
	}
	if input.Assurance == executionv1.ExecutionAssurance_EXECUTION_ASSURANCE_UNSPECIFIED {
		return BeginResult{}, fmt.Errorf("%w: execution assurance is required", ErrInvalid)
	}
	if !canonicalSHA256(input.OperationInputSHA256) {
		return BeginResult{}, fmt.Errorf("%w: operation input SHA-256 must be canonical", ErrInvalid)
	}
	resources, err := withOperationInput(input.OperationInputSHA256, input.Resources)
	if err != nil {
		return BeginResult{}, err
	}
	target := proto.Clone(input.Target).(*executionv1.ExecutionTargetV1)
	admission := Admission{
		OperationID:   execution.OperationID(),
		OperationKind: input.OperationKind,
		ProducerID:    r.producer.GetId(),
		Assurance:     input.Assurance,
		Target:        target,
	}
	claims, err := r.authority.Verify(ctx, execution.WorkContext(), admission)
	if err != nil {
		return BeginResult{}, fmt.Errorf("%w: verify Work Context: %v", ErrInvalid, err)
	}
	if claims == nil {
		return BeginResult{}, fmt.Errorf("%w: authority returned no Work Context claims", ErrInvalid)
	}
	claims = proto.Clone(claims).(*basev0.WorkContextV1)
	claimsWorkspace := claims.GetWorkspaceId()
	if claimsWorkspace == "" {
		return BeginResult{}, fmt.Errorf("%w: Work Context workspace is required", ErrInvalid)
	}
	if target.GetWorkspaceId() == "" {
		target.WorkspaceId = claimsWorkspace
	} else if target.GetWorkspaceId() != claimsWorkspace {
		return BeginResult{}, fmt.Errorf("%w: execution target workspace does not match Work Context", ErrInvalid)
	}
	if target.GetProjectId() == "" && claims.ProjectId != nil {
		projectID := claims.GetProjectId()
		target.ProjectId = &projectID
	} else if target.GetProjectId() != "" &&
		(claims.ProjectId == nil || target.GetProjectId() != claims.GetProjectId()) {
		return BeginResult{}, fmt.Errorf("%w: execution target project does not match Work Context", ErrInvalid)
	}
	if target.GetWorkspaceId() == "" || target.GetService() == "" {
		return BeginResult{}, fmt.Errorf("%w: resolved target requires workspace and service", ErrInvalid)
	}
	admission.Target = target
	attemptID := stableAttemptID(claims.GetTenantId(), r.producer.GetId(), execution.OperationID())
	tokenDigest := sha256.Sum256([]byte(execution.WorkContext().Encoded()))

	if existing, startedReceipt, found, err := r.findExisting(ctx, claims.GetTenantId(), execution.OperationID(), attemptID); err != nil {
		return BeginResult{}, err
	} else if found {
		if !sameAdmission(
			startedReceipt,
			claims,
			tokenDigest,
			admission,
			attemptID,
			r.producer,
			input.OperationInputSHA256,
		) {
			return BeginResult{}, fmt.Errorf("%w: operation identity was reused with different immutable facts", ErrConflict)
		}
		return BeginResult{Existing: &existing}, nil
	}

	started := &executionv1.ExecutionReceiptV1{
		Schema:            executionreceipt.SchemaV1,
		ReceiptId:         stableReceiptID(claims.GetTenantId(), r.producer.GetId(), execution.OperationID(), attemptID, executionv1.ExecutionStage_EXECUTION_STAGE_STARTED),
		OperationId:       execution.OperationID(),
		AttemptId:         attemptID,
		Stage:             executionv1.ExecutionStage_EXECUTION_STAGE_STARTED,
		OperationKind:     input.OperationKind,
		Producer:          proto.Clone(r.producer).(*executionv1.ExecutionProducerV1),
		Assurance:         input.Assurance,
		WorkContext:       claims,
		WorkContextSha256: hex.EncodeToString(tokenDigest[:]),
		Target:            proto.Clone(target).(*executionv1.ExecutionTargetV1),
		StartedAt:         timestamppb.New(r.now().UTC()),
		Resources:         resources,
	}
	attestation, err := r.attestor.Attest(started)
	if err != nil {
		return BeginResult{}, fmt.Errorf("attest STARTED execution receipt: %w", err)
	}
	if _, err := r.journal.Append(ctx, attestation); err != nil {
		if errors.Is(err, executionjournal.ErrConflict) {
			existing, startedReceipt, found, lookupErr := r.findExisting(ctx, claims.GetTenantId(), execution.OperationID(), attemptID)
			if lookupErr == nil && found &&
				sameAdmission(
					startedReceipt,
					claims,
					tokenDigest,
					admission,
					attemptID,
					r.producer,
					input.OperationInputSHA256,
				) {
				return BeginResult{Existing: &existing}, nil
			}
		}
		return BeginResult{}, fmt.Errorf("persist STARTED execution receipt: %w", err)
	}
	return BeginResult{
		Attempt: &Attempt{
			recorder: r,
			started:  attestation.GetReceipt(),
		},
	}, nil
}

// Finish durably appends exactly one terminal fact. The caller has already
// observed the effect; a returned journal error must be surfaced as
// reconciliation state and must not rewrite the effect's real result.
func (a *Attempt) Finish(
	ctx context.Context,
	input FinishInput,
) (*executionv1.ExecutionAttestationV1, error) {
	if a == nil || a.recorder == nil || a.started == nil {
		return nil, fmt.Errorf("%w: attempt is not initialized", ErrInvalid)
	}
	if !terminal(input.Stage) {
		return nil, fmt.Errorf("%w: finish stage must be terminal", ErrInvalid)
	}
	if input.Result == nil {
		return nil, fmt.Errorf("%w: terminal result is required", ErrInvalid)
	}
	operationInputSHA256, err := operationInputSHA256(a.started.GetResources())
	if err != nil {
		return nil, err
	}
	resources, err := withOperationInput(operationInputSHA256, input.Resources)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished {
		return nil, fmt.Errorf("%w: attempt was already finished in this process", ErrConflict)
	}

	completed := a.recorder.now().UTC()
	if completed.Before(a.started.GetStartedAt().AsTime()) {
		return nil, fmt.Errorf("%w: completion precedes start", ErrInvalid)
	}
	terminalReceipt := proto.Clone(a.started).(*executionv1.ExecutionReceiptV1)
	terminalReceipt.ReceiptId = stableReceiptID(
		terminalReceipt.GetWorkContext().GetTenantId(),
		terminalReceipt.GetProducer().GetId(),
		terminalReceipt.GetOperationId(),
		terminalReceipt.GetAttemptId(),
		input.Stage,
	)
	terminalReceipt.Stage = input.Stage
	terminalReceipt.CompletedAt = timestamppb.New(completed)
	terminalReceipt.Resources = resources
	terminalReceipt.Result = proto.Clone(input.Result).(*executionv1.ExecutionResultV1)
	terminalReceipt.PayloadSha256 = ""
	attestation, err := a.recorder.attestor.Attest(terminalReceipt)
	if err != nil {
		return nil, fmt.Errorf("attest terminal execution receipt: %w", err)
	}
	if _, err := a.recorder.journal.Append(ctx, attestation); err != nil {
		return nil, fmt.Errorf("persist terminal execution receipt: %w", err)
	}
	a.finished = true
	return attestation, nil
}

// RecoverIncomplete appends UNCERTAIN for every durable STARTED receipt with
// no terminal. Gateways call this before accepting new governed work.
func (r *Recorder) RecoverIncomplete(ctx context.Context, batchLimit int) (int, error) {
	if r == nil {
		return 0, fmt.Errorf("%w: recorder is nil", ErrInvalid)
	}
	if batchLimit < 1 || batchLimit > 1000 {
		return 0, fmt.Errorf("%w: recovery batch limit must be between 1 and 1000", ErrInvalid)
	}
	recovered := 0
	for {
		incomplete, err := r.journal.IncompleteAttempts(ctx, batchLimit)
		if err != nil {
			return recovered, err
		}
		for _, entry := range incomplete {
			started := entry.Attestation.GetReceipt()
			completed := r.now().UTC()
			if completed.Before(started.GetStartedAt().AsTime()) {
				completed = started.GetStartedAt().AsTime()
			}
			receipt := proto.Clone(started).(*executionv1.ExecutionReceiptV1)
			receipt.ReceiptId = stableReceiptID(
				receipt.GetWorkContext().GetTenantId(),
				receipt.GetProducer().GetId(),
				receipt.GetOperationId(),
				receipt.GetAttemptId(),
				executionv1.ExecutionStage_EXECUTION_STAGE_UNCERTAIN,
			)
			receipt.Stage = executionv1.ExecutionStage_EXECUTION_STAGE_UNCERTAIN
			receipt.CompletedAt = timestamppb.New(completed)
			receipt.Result = &executionv1.ExecutionResultV1{
				Status:     "uncertain",
				ErrorCode:  stringPointer("gateway-restarted-before-terminal"),
				DurationMs: durationMillis(started.GetStartedAt().AsTime(), completed),
			}
			receipt.PayloadSha256 = ""
			attestation, err := r.attestor.Attest(receipt)
			if err != nil {
				return recovered, fmt.Errorf("attest UNCERTAIN execution receipt: %w", err)
			}
			if _, err := r.journal.Append(ctx, attestation); err != nil {
				return recovered, fmt.Errorf("persist UNCERTAIN execution receipt: %w", err)
			}
			recovered++
		}
		if len(incomplete) < batchLimit {
			return recovered, nil
		}
	}
}

func (r *Recorder) findExisting(
	ctx context.Context,
	tenantID string,
	operationID string,
	attemptID string,
) (executionjournal.Entry, *executionv1.ExecutionReceiptV1, bool, error) {
	startedID := stableReceiptID(
		tenantID,
		r.producer.GetId(),
		operationID,
		attemptID,
		executionv1.ExecutionStage_EXECUTION_STAGE_STARTED,
	)
	started, found, err := r.journal.Lookup(ctx, startedID)
	if err != nil {
		return executionjournal.Entry{}, nil, false, err
	}
	if !found {
		return executionjournal.Entry{}, nil, false, nil
	}
	for _, stage := range []executionv1.ExecutionStage{
		executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED,
		executionv1.ExecutionStage_EXECUTION_STAGE_FAILED,
		executionv1.ExecutionStage_EXECUTION_STAGE_COMPENSATED,
		executionv1.ExecutionStage_EXECUTION_STAGE_UNCERTAIN,
	} {
		receiptID := stableReceiptID(tenantID, r.producer.GetId(), operationID, attemptID, stage)
		entry, found, err := r.journal.Lookup(ctx, receiptID)
		if err != nil {
			return executionjournal.Entry{}, nil, false, err
		}
		if found {
			return entry, started.Attestation.GetReceipt(), true, nil
		}
	}
	return started, started.Attestation.GetReceipt(), true, nil
}

func sameAdmission(
	receipt *executionv1.ExecutionReceiptV1,
	claims *basev0.WorkContextV1,
	tokenDigest [sha256.Size]byte,
	admission Admission,
	attemptID string,
	producer *executionv1.ExecutionProducerV1,
	operationInputDigest string,
) bool {
	if receipt == nil {
		return false
	}
	storedInputDigest, err := operationInputSHA256(receipt.GetResources())
	return err == nil &&
		storedInputDigest == operationInputDigest &&
		receipt.GetOperationId() == admission.OperationID &&
		receipt.GetAttemptId() == attemptID &&
		receipt.GetOperationKind() == admission.OperationKind &&
		receipt.GetAssurance() == admission.Assurance &&
		receipt.GetWorkContextSha256() == hex.EncodeToString(tokenDigest[:]) &&
		proto.Equal(receipt.GetWorkContext(), claims) &&
		proto.Equal(receipt.GetProducer(), producer) &&
		proto.Equal(receipt.GetTarget(), admission.Target)
}

func stableAttemptID(tenantID, producerID, operationID string) string {
	sum := digestParts("attempt", tenantID, producerID, operationID)
	return "attempt-" + hex.EncodeToString(sum[:])
}

func stableReceiptID(
	tenantID string,
	producerID string,
	operationID string,
	attemptID string,
	stage executionv1.ExecutionStage,
) string {
	sum := digestParts("receipt", tenantID, producerID, operationID, attemptID, fmt.Sprint(int32(stage)))
	return "receipt-" + hex.EncodeToString(sum[:])
}

func digestParts(parts ...string) [sha256.Size]byte {
	hash := sha256.New()
	for _, part := range parts {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(part))
	}
	var sum [sha256.Size]byte
	copy(sum[:], hash.Sum(nil))
	return sum
}

func cloneResources(resources []*executionv1.ExecutionResourceV1) []*executionv1.ExecutionResourceV1 {
	if resources == nil {
		return nil
	}
	return proto.Clone(&executionv1.ExecutionReceiptV1{Resources: resources}).(*executionv1.ExecutionReceiptV1).Resources
}

const operationInputResourceKind = "operation.input"

func withOperationInput(
	inputSHA256 string,
	resources []*executionv1.ExecutionResourceV1,
) ([]*executionv1.ExecutionResourceV1, error) {
	if !canonicalSHA256(inputSHA256) {
		return nil, fmt.Errorf("%w: operation input SHA-256 must be canonical", ErrInvalid)
	}
	if len(resources) > 127 {
		return nil, fmt.Errorf("%w: at most 127 observed resources are supported", ErrInvalid)
	}
	cloned := cloneResources(resources)
	for _, resource := range cloned {
		if resource.GetKind() == operationInputResourceKind {
			return nil, fmt.Errorf("%w: resource kind %q is recorder-owned", ErrInvalid, operationInputResourceKind)
		}
	}
	return append([]*executionv1.ExecutionResourceV1{{
		Kind:      operationInputResourceKind,
		Reference: "sha256:" + inputSHA256,
	}}, cloned...), nil
}

func operationInputSHA256(resources []*executionv1.ExecutionResourceV1) (string, error) {
	var inputSHA256 string
	for _, resource := range resources {
		if resource.GetKind() != operationInputResourceKind {
			continue
		}
		if inputSHA256 != "" {
			return "", fmt.Errorf("%w: multiple operation input resources", ErrInvalid)
		}
		const prefix = "sha256:"
		reference := resource.GetReference()
		if len(reference) <= len(prefix) || reference[:len(prefix)] != prefix {
			return "", fmt.Errorf("%w: malformed operation input resource", ErrInvalid)
		}
		inputSHA256 = reference[len(prefix):]
	}
	if !canonicalSHA256(inputSHA256) {
		return "", fmt.Errorf("%w: missing canonical operation input resource", ErrInvalid)
	}
	return inputSHA256, nil
}

func canonicalSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func terminal(stage executionv1.ExecutionStage) bool {
	switch stage {
	case executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED,
		executionv1.ExecutionStage_EXECUTION_STAGE_FAILED,
		executionv1.ExecutionStage_EXECUTION_STAGE_COMPENSATED,
		executionv1.ExecutionStage_EXECUTION_STAGE_UNCERTAIN:
		return true
	default:
		return false
	}
}

func durationMillis(started, completed time.Time) uint64 {
	if completed.Before(started) {
		return 0
	}
	return uint64(completed.Sub(started) / time.Millisecond)
}

func stringPointer(value string) *string {
	return &value
}
