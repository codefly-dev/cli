package executionrecorder

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/workcontext"
	"google.golang.org/grpc/metadata"
)

// The gRPC metadata keys the SDK's clients send. They are transport — which
// keys carry the capability and the operation id to an execution boundary —
// and say nothing about the capability's wire form, which is core's alone
// (core/workcontext is the one implementation that mints and verifies it).
const (
	WorkContextMetadataKey = "x-codefly-work-context"
	OperationIDMetadataKey = "x-codefly-operation-id"
	maxOperationIDBytes    = 128
)

// ExecutionContext is the opaque capability and the caller-stable logical
// operation identity carried to a Codefly execution boundary. The capability
// is verified by the Authority at Begin, never here: this type holds it only
// to its size bound.
type ExecutionContext struct {
	workContext string
	operationID string
}

// NewExecutionContext freezes one capability/operation pair.
func NewExecutionContext(workContext, operationID string) (ExecutionContext, error) {
	if workContext == "" {
		return ExecutionContext{}, fmt.Errorf("%w: empty Work Context", ErrInvalid)
	}
	if len(workContext) > workcontext.MaxTokenSize {
		return ExecutionContext{}, fmt.Errorf("%w: Work Context exceeds %d bytes", ErrInvalid, workcontext.MaxTokenSize)
	}
	if err := validateOperationID(operationID); err != nil {
		return ExecutionContext{}, err
	}
	return ExecutionContext{workContext: workContext, operationID: operationID}, nil
}

// WorkContext is the encoded capability, verified by nobody yet.
func (execution ExecutionContext) WorkContext() string { return execution.workContext }

// OperationID is the caller-stable logical operation identifier.
func (execution ExecutionContext) OperationID() string { return execution.operationID }

// WithGRPCExecutionContext attaches one execution context to the outgoing gRPC
// metadata while keeping unrelated metadata. A carrier already present is
// refused rather than overwritten or joined.
func WithGRPCExecutionContext(ctx context.Context, execution ExecutionContext) (context.Context, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil gRPC context", ErrInvalid)
	}
	validated, err := NewExecutionContext(execution.workContext, execution.operationID)
	if err != nil {
		return nil, err
	}
	existing, _ := metadata.FromOutgoingContext(ctx)
	if len(existing.Get(WorkContextMetadataKey)) != 0 {
		return nil, fmt.Errorf("%w: outgoing gRPC Work Context already set", ErrInvalid)
	}
	if len(existing.Get(OperationIDMetadataKey)) != 0 {
		return nil, fmt.Errorf("%w: outgoing gRPC operation ID already set", ErrInvalid)
	}
	return metadata.AppendToOutgoingContext(ctx,
		WorkContextMetadataKey, validated.workContext,
		OperationIDMetadataKey, validated.operationID,
	), nil
}

// ExecutionContextFromIncoming reads the execution context off the incoming
// gRPC metadata: exactly one value for each key, and nothing about trust.
func ExecutionContextFromIncoming(ctx context.Context) (ExecutionContext, error) {
	if ctx == nil {
		return ExecutionContext{}, fmt.Errorf("%w: nil gRPC context", ErrInvalid)
	}
	values, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ExecutionContext{}, fmt.Errorf("%w: missing incoming gRPC metadata", ErrInvalid)
	}
	workContexts := values.Get(WorkContextMetadataKey)
	if len(workContexts) != 1 {
		return ExecutionContext{}, fmt.Errorf("%w: incoming gRPC Work Context requires exactly one value", ErrInvalid)
	}
	operationIDs := values.Get(OperationIDMetadataKey)
	if len(operationIDs) != 1 {
		return ExecutionContext{}, fmt.Errorf("%w: incoming gRPC operation ID requires exactly one value", ErrInvalid)
	}
	return NewExecutionContext(workContexts[0], operationIDs[0])
}

// ExecutionContextFromIncomingIfPresent is for boundaries where execution
// attribution is optional: no carrier at all is present=false, and a partial
// or duplicate carrier is still an error.
func ExecutionContextFromIncomingIfPresent(ctx context.Context) (execution ExecutionContext, present bool, err error) {
	if ctx == nil {
		return ExecutionContext{}, false, fmt.Errorf("%w: nil gRPC context", ErrInvalid)
	}
	values, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ExecutionContext{}, false, nil
	}
	if len(values.Get(WorkContextMetadataKey)) == 0 && len(values.Get(OperationIDMetadataKey)) == 0 {
		return ExecutionContext{}, false, nil
	}
	execution, err = ExecutionContextFromIncoming(ctx)
	if err != nil {
		return ExecutionContext{}, false, err
	}
	return execution, true, nil
}

func validateOperationID(operationID string) error {
	if operationID == "" {
		return fmt.Errorf("%w: operation ID is required", ErrInvalid)
	}
	if strings.TrimSpace(operationID) != operationID {
		return fmt.Errorf("%w: operation ID is not canonical", ErrInvalid)
	}
	if len(operationID) > maxOperationIDBytes {
		return fmt.Errorf("%w: operation ID exceeds %d bytes", ErrInvalid, maxOperationIDBytes)
	}
	for _, character := range operationID {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.', character == ':':
		default:
			return fmt.Errorf("%w: operation ID contains unsupported characters", ErrInvalid)
		}
	}
	return nil
}
