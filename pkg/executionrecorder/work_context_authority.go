package executionrecorder

import (
	"context"
	"fmt"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	workcontext "github.com/codefly-dev/sdk-go/workcontext"
)

const (
	// ExecutionWorkContextAudience is the exact Gateway trust boundary.
	ExecutionWorkContextAudience = "codefly.execution"

	executionEvidenceResourceKind = "evidence"
	executionEvidenceAction       = "append"
)

// WorkContextAuthorityConfig binds one Accounts issuer and exact audience to
// the Gateway recorder.
type WorkContextAuthorityConfig struct {
	Issuer   string
	Audience string
}

// WorkContextAuthority authorizes an ALREADY-VERIFIED capability for one
// producer's evidence, and refuses when there is none.
//
// It does not verify, and there is no seam where verification could be added
// back. Verification belongs to whoever can answer the four live questions
// core's Verifier requires — the authorization revision, the replay store, the
// grant source and the SEAL source — and this repository can answer none of
// them. sdk-go's own core.go states the consequence: "A service that cannot
// answer those four is not in a position to verify a capability itself; it
// presents its own and lets the component that holds them decide."
//
// The JWKS verifier that stood here was from the JSON era. Keeping a seam for
// it would have been worse than deleting it: a verifier handed invented
// revision, grant or seal state does not fail, it PASSES — the one outcome the
// seal check exists to prevent. So a caller without a *workcontext.Verified is
// refused, never served by a weaker check.
//
// What remains is what this boundary genuinely owns: that the verified
// capability was minted for THIS issuer and audience, and that the final
// actor's effective authority names this producer's evidence EXPLICITLY.
type WorkContextAuthority struct {
	issuer   string
	audience string
}

// NewWorkContextAuthority creates a fail-closed recorder authority.
func NewWorkContextAuthority(config WorkContextAuthorityConfig) (*WorkContextAuthority, error) {
	if strings.TrimSpace(config.Issuer) == "" {
		return nil, fmt.Errorf("%w: Work Context issuer is required", ErrInvalid)
	}
	if strings.TrimSpace(config.Audience) == "" {
		return nil, fmt.Errorf("%w: Work Context audience is required", ErrInvalid)
	}
	return &WorkContextAuthority{issuer: config.Issuer, audience: config.Audience}, nil
}

// Authorize implements Authority.
func (a *WorkContextAuthority) Authorize(
	_ context.Context,
	verified *workcontext.Verified,
	admission Admission,
) error {
	if a == nil {
		return fmt.Errorf("%w: Work Context authority is not initialized", ErrInvalid)
	}
	if verified == nil {
		return fmt.Errorf("%w: no verified Work Context was supplied; this component does not verify capabilities and will not accept an unverified one", ErrInvalid)
	}
	if strings.TrimSpace(admission.ProducerID) == "" {
		return fmt.Errorf("%w: execution producer ID is required", ErrInvalid)
	}
	claims := verified.Context()
	if claims.GetIssuer() != a.issuer {
		return fmt.Errorf("%w: capability was minted by issuer %q, not %q", ErrInvalid, claims.GetIssuer(), a.issuer)
	}
	if claims.GetAudience() != a.audience {
		return fmt.Errorf("%w: capability was minted for audience %q, not %q", ErrInvalid, claims.GetAudience(), a.audience)
	}
	if err := requireExplicitEvidenceScope(verified.EffectiveScopes(), admission.ProducerID); err != nil {
		return fmt.Errorf("authorize execution evidence producer: %w", err)
	}
	return nil
}

// requireExplicitEvidenceScope demands that the final actor's effective
// authority name this producer's evidence by id.
//
// EXPLICITLY: a scope listing no resource ids, or a wildcard, does not append
// evidence for this producer. Evidence is the record of what ran, so authority
// over it is granted per producer or not at all — a capability that may append
// "evidence" everywhere is how one producer's receipts get written under
// another's name. core's Verified exposes no scope predicate, so the rule
// lives here, where it is enforced, rather than in a second place that could
// drift from it.
func requireExplicitEvidenceScope(scopes []*basev0.WorkScopeV1, producerID string) error {
	for _, scope := range scopes {
		if scope.GetResourceKind() != executionEvidenceResourceKind {
			continue
		}
		if !namesExactly(scope.GetActions(), executionEvidenceAction) {
			continue
		}
		if namesExactly(scope.GetResourceIds(), producerID) {
			return nil
		}
	}
	return fmt.Errorf("%w: no effective scope appends %q evidence for producer %q by name",
		ErrInvalid, executionEvidenceAction, producerID)
}

func namesExactly(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
