package executionrecorder

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/workcontext"
)

const (
	// ExecutionWorkContextAudience is the exact Gateway trust boundary.
	ExecutionWorkContextAudience = "codefly.execution"

	executionEvidenceResourceKind = "evidence"
	executionEvidenceAction       = "append"
)

// Authenticator is core's, by alias. The Work Context is a core proto and
// core/workcontext is its one implementation — the only mint and the only
// verify; this package resolves to it rather than carrying a second one, and
// the assertion below holds the alias to core's type at compile time so the
// resolution cannot drift into a local copy. A party that verifies without
// minting — this gateway — is what Authenticator is for: it assembles core's
// Verifier and calls Verify, so there is one check path, and a capability
// carrying a grant hop is refused (ErrNeedsIssuer) rather than accepted
// unchecked.
type Authenticator = workcontext.Authenticator

var _ interface {
	Authenticate(context.Context, string) (*workcontext.Authenticated, error)
} = (*Authenticator)(nil)

// WorkContextAuthorityConfig binds one issuer and exact audience to the
// gateway recorder, with the LIVE state core's authenticator verifies every
// capability against. None of it is optional: core refuses to assemble a
// verifier without a seal source or a revision source, because a verifier
// that could not tell a capability sealed to a superseded installation from
// a current one would make the strongest check in the model the easiest one
// to omit.
type WorkContextAuthorityConfig struct {
	Issuer   string
	Audience string
	// Keys are the issuer's published verification keys (a JWKS, or a fixed
	// set).
	Keys KeySource
	// Revisions is the issuer's live authorization revision, per tenant.
	Revisions workcontext.RevisionSource
	// Seals is the issuer's live sealed state: installation, principal
	// epoch, approved build and operation binding.
	Seals workcontext.SealSource
	// Replay consumes single-use capabilities. nil is a process-local store:
	// a capability is consumed once per gateway process, which is the store
	// a single gateway can hold.
	Replay workcontext.ReplayStore
	// Now is the clock, for tests.
	Now func() time.Time
	// TrustTheConformanceFixtureKey is copied into core's authenticator by
	// the conformance test and nowhere else: the kit's key is derivable from
	// core's source, and a production authenticator refuses it by default.
	TrustTheConformanceFixtureKey bool
}

// WorkContextAuthority verifies identity with core's authenticator and
// requires the actor's effective evidence authority to name this producer.
type WorkContextAuthority struct {
	issuer    string
	audience  string
	keys      KeySource
	revisions workcontext.RevisionSource
	seals     workcontext.SealSource
	replay    workcontext.ReplayStore
	now       func() time.Time
	trustKit  bool
}

// NewWorkContextAuthority creates a fail-closed recorder authority.
func NewWorkContextAuthority(config *WorkContextAuthorityConfig) (*WorkContextAuthority, error) {
	switch {
	case config == nil:
		return nil, fmt.Errorf("%w: Work Context authority configuration is required", ErrInvalid)
	case strings.TrimSpace(config.Issuer) == "":
		return nil, fmt.Errorf("%w: Work Context issuer is required", ErrInvalid)
	case strings.TrimSpace(config.Audience) == "":
		return nil, fmt.Errorf("%w: Work Context audience is required", ErrInvalid)
	case config.Keys == nil:
		return nil, fmt.Errorf("%w: Work Context key source is required", ErrInvalid)
	case config.Revisions == nil:
		return nil, fmt.Errorf("%w: Work Context authorization-revision source is required; core's authenticator has no mode without the issuer's live revision", ErrInvalid)
	case config.Seals == nil:
		return nil, fmt.Errorf("%w: Work Context seal source is required; the seal comparison is never skipped", ErrInvalid)
	}
	replay := config.Replay
	if replay == nil {
		store := workcontext.NewMemoryReplayStore()
		if config.Now != nil {
			store.Now = config.Now
		}
		replay = store
	}
	return &WorkContextAuthority{
		issuer: config.Issuer, audience: config.Audience, keys: config.Keys,
		revisions: config.Revisions, seals: config.Seals, replay: replay,
		now: config.Now, trustKit: config.TrustTheConformanceFixtureKey,
	}, nil
}

// Authenticate is the verification entrypoint, and it is core's: the keys the
// issuer publishes now are handed to core's Authenticator, which makes every
// check — issuer, signature, audience, window, structure, authorization
// revision, seal, epoch, binding, build, replay. The key id the capability
// names is read through core's Inspect only to let the key source refresh on
// a key it does not hold; Inspect's verdict is not used, so a capability it
// refuses is refused by Verify, with Verify's reason.
func (a *WorkContextAuthority) Authenticate(ctx context.Context, encoded string) (*workcontext.Authenticated, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: Work Context authority is not initialized", ErrInvalid)
	}
	keyID := ""
	if inspected, err := workcontext.Inspect(encoded); err == nil {
		keyID = inspected.Context().GetKeyId()
	}
	keys, err := a.keys.Keys(ctx, keyID)
	if err != nil {
		return nil, fmt.Errorf("work context verification keys: %w", err)
	}
	authenticator := &Authenticator{
		Issuer: a.issuer, Audience: a.audience, Keys: keys,
		Revisions: a.revisions, Seals: a.seals, Replay: a.replay,
		Now: a.now, TrustTheConformanceFixtureKey: a.trustKit,
	}
	return authenticator.Authenticate(ctx, encoded)
}

// Verify implements Authority: the capability is authenticated by core, and
// the actor's effective scopes must grant evidence/append on THIS producer,
// named explicitly.
func (a *WorkContextAuthority) Verify(ctx context.Context, encoded string, admission Admission) (*basev0.WorkContextV1, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: Work Context authority is not initialized", ErrInvalid)
	}
	if strings.TrimSpace(admission.ProducerID) == "" {
		return fmt.Errorf("%w: execution producer ID is required", ErrInvalid)
	}
	authenticated, err := a.Authenticate(ctx, encoded)
	if err != nil {
		return nil, fmt.Errorf("verify Work Context: %w", err)
	}
	if err := requireExplicitScope(authenticated.EffectiveScopes(), executionEvidenceResourceKind, executionEvidenceAction, admission.ProducerID); err != nil {
		return nil, fmt.Errorf("authorize execution evidence producer: %w", err)
	}
	return authenticated.Context(), nil
}

// requireExplicitScope holds the actor's effective scopes to one action on one
// NAMED resource. Under core's containment rule a scope naming no resource ids
// is a wildcard; the recorder does not take a wildcard for the evidence it
// appends — the producer must be named, so a capability minted for "any
// evidence" does not reach this one's journal.
func requireExplicitScope(scopes []*basev0.WorkScopeV1, kind, action, resourceID string) error {
	for _, scope := range scopes {
		if scope.GetResourceKind() != kind || !slices.Contains(scope.GetActions(), action) {
			continue
		}
		if slices.Contains(scope.GetResourceIds(), resourceID) {
			return nil
		}
	}
	return fmt.Errorf("%w: the capability grants no %s %s naming %q", ErrInvalid, kind, action, resourceID)
}
