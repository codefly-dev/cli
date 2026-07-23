package executionrecorder

import (
	"context"
	"fmt"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	codefly "github.com/codefly-dev/sdk-go"
)

const (
	// ExecutionWorkContextAudience is the exact Gateway trust boundary.
	ExecutionWorkContextAudience = "codefly.execution"

	executionEvidenceResourceKind = "evidence"
	executionEvidenceAction       = "append"
)

// WorkContextVerifier is implemented by the SDK's rotation-aware JWKS
// verifier. Keeping this interface narrow makes authority policy testable
// without moving token or key semantics out of the SDK.
type WorkContextVerifier interface {
	Verify(
		context.Context,
		codefly.WorkContextToken,
		codefly.WorkContextExpectations,
	) (*basev0.WorkContextV1, error)
}

// WorkContextAuthorityConfig binds one Accounts issuer and exact audience to
// the Gateway recorder.
type WorkContextAuthorityConfig struct {
	Verifier WorkContextVerifier
	Issuer   string
	Audience string
}

// WorkContextAuthority verifies identity with the SDK and requires the final
// actor's effective evidence authority to be explicitly producer-bound.
type WorkContextAuthority struct {
	verifier WorkContextVerifier
	issuer   string
	audience string
}

// NewWorkContextAuthority creates a fail-closed recorder authority.
func NewWorkContextAuthority(config WorkContextAuthorityConfig) (*WorkContextAuthority, error) {
	if config.Verifier == nil {
		return nil, fmt.Errorf("%w: Work Context verifier is required", ErrInvalid)
	}
	if strings.TrimSpace(config.Issuer) == "" {
		return nil, fmt.Errorf("%w: Work Context issuer is required", ErrInvalid)
	}
	if strings.TrimSpace(config.Audience) == "" {
		return nil, fmt.Errorf("%w: Work Context audience is required", ErrInvalid)
	}
	return &WorkContextAuthority{
		verifier: config.Verifier,
		issuer:   config.Issuer,
		audience: config.Audience,
	}, nil
}

// Verify implements Authority.
func (a *WorkContextAuthority) Verify(
	ctx context.Context,
	token codefly.WorkContextToken,
	admission Admission,
) (*basev0.WorkContextV1, error) {
	if a == nil || a.verifier == nil {
		return nil, fmt.Errorf("%w: Work Context authority is not initialized", ErrInvalid)
	}
	if strings.TrimSpace(admission.ProducerID) == "" {
		return nil, fmt.Errorf("%w: execution producer ID is required", ErrInvalid)
	}
	claims, err := a.verifier.Verify(ctx, token, codefly.WorkContextExpectations{
		Issuer:   a.issuer,
		Audience: a.audience,
	})
	if err != nil {
		return nil, fmt.Errorf("verify SDK Work Context: %w", err)
	}
	if err := codefly.RequireWorkContextScope(claims, codefly.WorkContextScopeRequirement{
		ResourceKind:            executionEvidenceResourceKind,
		Action:                  executionEvidenceAction,
		ResourceID:              admission.ProducerID,
		RequireExplicitResource: true,
	}); err != nil {
		return nil, fmt.Errorf("authorize execution evidence producer: %w", err)
	}
	return claims, nil
}
