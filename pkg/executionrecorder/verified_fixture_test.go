package executionrecorder

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	coreworkcontext "github.com/codefly-dev/core/workcontext"
	"github.com/codefly-dev/core/workcontext/conformance"
	workcontext "github.com/codefly-dev/sdk-go/workcontext"
)

// fixtureNow is the clock the conformance kit's capabilities are minted under.
const fixtureYear = 2026

// verifiedFixture returns a capability that core's own Verifier accepted.
//
// It exists because a *workcontext.Verified cannot be hand-built — only
// Verifier.Verify constructs one — which is the point of the cutover and not an
// inconvenience: a test that could fabricate one would be testing a path
// production cannot reach. The conformance kit is core's supported way to get a
// real one, with the fixture key the verifier trusts only when asked.
//
// The recorder tests pair this with a stub Authority: the capability is real,
// so the receipt's digest is real, while the authorization DECISION is the
// stub's. That split is deliberate — those tests are about journalling and
// replay, and the authorization rules have their own tests over
// requireExplicitEvidenceScope.
func verifiedFixture(t *testing.T) *workcontext.Verified {
	t.Helper()
	verified, err := conformanceVerified(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return verified
}

// conformanceVerified is verifiedFixture for a caller with no *testing.T — the
// process-loss test re-execs itself as a child, and that child must obtain its
// capability the same way rather than inventing one.
func conformanceVerified(ctx context.Context) (*workcontext.Verified, error) {
	now := time.Date(fixtureYear, time.July, 23, 19, 0, 0, 0, time.UTC)
	verifier := conformance.New(now).Verifier()
	fixtures, err := coreworkcontext.Fixtures(now)
	if err != nil {
		return nil, fmt.Errorf("core's conformance fixtures: %w", err)
	}
	// Only an ACCEPTED fixture is a capability a caller would present; the kit
	// includes rejected ones on purpose, to exercise refusals.
	for _, fixture := range fixtures {
		if fixture.Outcome != coreworkcontext.OutcomeAccepted {
			continue
		}
		verified, verifyErr := verifier.Verify(ctx, fixture.Token)
		if verifyErr == nil {
			return verified, nil
		}
	}
	return nil, errors.New("core's conformance kit produced no capability its own verifier accepts")
}
