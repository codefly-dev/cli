package executionrecorder

import (
	"errors"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
)

// A nil verified capability is refused by name.
//
// This is the whole shape of the cutover: the authority has no verifier and no
// token parameter, so the only thing that can reach it is something core's
// Verifier produced. A caller that cannot produce one is refused rather than
// served by a weaker check.
func TestAuthorizeRefusesWithoutAVerifiedCapability(t *testing.T) {
	authority, err := NewWorkContextAuthority(WorkContextAuthorityConfig{
		Issuer: "accounts", Audience: ExecutionWorkContextAudience,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = authority.Authorize(t.Context(), nil, Admission{ProducerID: "codefly.execution"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "does not verify capabilities") {
		t.Fatalf("the refusal must say why it cannot accept one: %q", err)
	}
}

// A capability that really was verified, but minted for another issuer or
// audience, is refused — with a *Verified only core can construct.
func TestAuthorizeRefusesAnotherIssuerOrAudience(t *testing.T) {
	verified := verifiedFixture(t)
	claims := verified.Context()

	for _, blocked := range []struct {
		name             string
		issuer, audience string
		want             string
	}{
		{"another issuer", "someone-else", claims.GetAudience(), "minted by issuer"},
		{"another audience", claims.GetIssuer(), "codefly.elsewhere", "minted for audience"},
	} {
		t.Run(blocked.name, func(t *testing.T) {
			authority, err := NewWorkContextAuthority(WorkContextAuthorityConfig{
				Issuer: blocked.issuer, Audience: blocked.audience,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = authority.Authorize(t.Context(), verified, Admission{ProducerID: "codefly.execution"})
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), blocked.want) {
				t.Fatalf("want a %q refusal, got %v", blocked.want, err)
			}
		})
	}
}

// A verified capability for the right issuer and audience is still refused when
// its effective authority does not name this producer's evidence — so the two
// checks are independent and neither stands in for the other.
func TestAuthorizeRefusesAVerifiedCapabilityWithoutEvidenceAuthority(t *testing.T) {
	verified := verifiedFixture(t)
	claims := verified.Context()
	authority, err := NewWorkContextAuthority(WorkContextAuthorityConfig{
		Issuer: claims.GetIssuer(), Audience: claims.GetAudience(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = authority.Authorize(t.Context(), verified, Admission{ProducerID: "codefly.execution"})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "appends") {
		t.Fatalf("want an evidence-authority refusal, got %v", err)
	}
}

// Evidence authority is granted per producer BY NAME. A wildcard, an empty
// resource list, or another producer's id does not append this producer's
// evidence — evidence is the record of what ran, so a capability that may
// append it anywhere is how one producer's receipts get written under
// another's name.
func TestExplicitEvidenceScopeIsRequiredByName(t *testing.T) {
	const producer = "codefly.execution"
	scope := func(kind string, actions, ids []string) *basev0.WorkScopeV1 {
		return &basev0.WorkScopeV1{ResourceKind: kind, Actions: actions, ResourceIds: ids}
	}
	for _, test := range []struct {
		name   string
		scopes []*basev0.WorkScopeV1
		allow  bool
	}{
		{"named explicitly", []*basev0.WorkScopeV1{scope("evidence", []string{"append"}, []string{producer})}, true},
		{"named beside others", []*basev0.WorkScopeV1{scope("evidence", []string{"append"}, []string{"other", producer})}, true},
		{"wildcard", []*basev0.WorkScopeV1{scope("evidence", []string{"append"}, []string{"*"})}, false},
		{"no resource ids", []*basev0.WorkScopeV1{scope("evidence", []string{"append"}, nil)}, false},
		{"another producer", []*basev0.WorkScopeV1{scope("evidence", []string{"append"}, []string{"other"})}, false},
		{"another action", []*basev0.WorkScopeV1{scope("evidence", []string{"read"}, []string{producer})}, false},
		{"another kind", []*basev0.WorkScopeV1{scope("artifact", []string{"append"}, []string{producer})}, false},
		{"nothing at all", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := requireExplicitEvidenceScope(test.scopes, producer)
			if test.allow && err != nil {
				t.Fatalf("want permitted, got %v", err)
			}
			if !test.allow && err == nil {
				t.Fatal("want refused, got permitted")
			}
		})
	}
}
