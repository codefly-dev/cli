package executionrecorder

import (
	"context"
	"fmt"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	coreworkcontext "github.com/codefly-dev/core/workcontext"
	"github.com/codefly-dev/core/workcontext/conformance"
	workcontext "github.com/codefly-dev/sdk-go/workcontext"
)

// The workspace and project a minted test capability names. They are the
// capability's, not the caller's: the recorder resolves an execution target's
// workspace FROM the capability and refuses a target that names another, so a
// test asserting that cannot also be the authority on the value.
const (
	fixtureWorkspaceID = "workspace-conformance"
	fixtureProjectID   = "project-conformance"
	fixtureProducerID  = "codefly.execution"
)

// fixtureNow is the clock the conformance key, seals and revisions are pinned
// to. A minter and a verifier reading different clocks refuse each other's
// sound capabilities, so there is one constant.
func fixtureNow() time.Time {
	return time.Date(2026, time.July, 23, 19, 0, 0, 0, time.UTC)
}

// verifiedFixture returns a capability core's own Verifier accepted, naming
// fixtureWorkspaceID and carrying this producer's evidence authority.
//
// It MINTS rather than replaying a conformance fixture. The kit's shipped
// fixtures name no workspace and carry record/read scopes, so a recorder test
// built on one could only pass with a stub authority handing back claims of
// its own — which is the hole round 15 found: claims from a second source let
// a receipt attest to a workspace the capability never named. Minting closes
// it at the root. Both ends here are core's single implementation — its
// Authority mints, its Verifier verifies — with the kit's published key pair,
// so nothing is fabricated and the recorder runs against the REAL
// WorkContextAuthority.
func verifiedFixture(t *testing.T) *workcontext.Verified {
	t.Helper()
	verified, err := mintedCapability(t.Context(), evidenceScope(fixtureProducerID))
	if err != nil {
		t.Fatal(err)
	}
	return verified
}

// evidenceScope is the authority the recorder demands by name: append evidence
// for exactly this producer. Spelled out here so a test granting something
// weaker (no resource ids, a wildcard, another producer) has to say so.
func evidenceScope(producerID string) []*basev0.WorkScopeV1 {
	return []*basev0.WorkScopeV1{{
		ResourceKind: executionEvidenceResourceKind,
		Actions:      []string{executionEvidenceAction},
		ResourceIds:  []string{producerID},
	}}
}

// mintedCapability is verifiedFixture for a caller with no *testing.T — the
// process-loss test re-execs itself as a child, and that child must obtain its
// capability the same way rather than inventing one — and for tests varying
// the scopes.
func mintedCapability(
	ctx context.Context,
	scopes []*basev0.WorkScopeV1,
) (*workcontext.Verified, error) {
	now := fixtureNow()
	settings := conformance.New(now)
	_, private := coreworkcontext.FixtureKeyPair()
	authority := &coreworkcontext.Authority{
		Issuer:    coreworkcontext.FixtureIssuer,
		KeyID:     coreworkcontext.FixtureKeyID,
		Key:       private,
		Revisions: coreworkcontext.FixtureRevisions(),
		Seals:     coreworkcontext.FixtureSeals(),
		Now:       func() time.Time { return now },
	}
	token, _, err := authority.Start(ctx, coreworkcontext.StartInput{
		TenantID:           coreworkcontext.FixtureTenant,
		OwnerPrincipalID:   coreworkcontext.FixturePrincipal,
		OwnerPrincipalKind: "human",
		OrganizationID:     coreworkcontext.FixtureOrganization,
		TaskID:             coreworkcontext.FixtureTask,
		Audience:           coreworkcontext.FixtureAudience,
		AuthorityScopes:    scopes,
		WorkspaceID:        fixtureWorkspaceID,
		ProjectID:          fixtureProjectID,
		InstallationID:     coreworkcontext.FixtureInstallation,
		Execution: coreworkcontext.Execution{
			ImageDigest:      coreworkcontext.FixtureImageDigest,
			BuildIncarnation: coreworkcontext.FixtureBuildIncarnation,
		},
		TTL: time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("mint a test capability: %w", err)
	}
	verified, err := settings.Verifier().Verify(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("core's verifier refused a capability core minted: %w", err)
	}
	return verified, nil
}

// fixtureAuthority is the REAL recorder authority, bound to the issuer and
// audience the minted capability carries.
func fixtureAuthority(t *testing.T) *WorkContextAuthority {
	t.Helper()
	authority, err := NewWorkContextAuthority(WorkContextAuthorityConfig{
		Issuer:   coreworkcontext.FixtureIssuer,
		Audience: coreworkcontext.FixtureAudience,
	})
	if err != nil {
		t.Fatal(err)
	}
	return authority
}
