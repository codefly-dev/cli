package executionrecorder

import (
	"context"
	"errors"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	codefly "github.com/codefly-dev/sdk-go"
	"google.golang.org/protobuf/proto"
)

type workContextVerifierFunc func(
	context.Context,
	codefly.WorkContextToken,
	codefly.WorkContextExpectations,
) (*basev0.WorkContextV1, error)

func (fn workContextVerifierFunc) Verify(
	ctx context.Context,
	token codefly.WorkContextToken,
	expected codefly.WorkContextExpectations,
) (*basev0.WorkContextV1, error) {
	return fn(ctx, token, expected)
}

func TestWorkContextAuthorityUsesSDKVerificationAndExactProducerScope(t *testing.T) {
	claims := testClaims()
	claims.AuthorityScopes = []*basev0.WorkScopeV1{{
		ResourceKind: "evidence",
		Actions:      []string{"append"},
		ResourceIds:  []string{"codefly.execution"},
	}}
	var verified bool
	authority, err := NewWorkContextAuthority(WorkContextAuthorityConfig{
		Issuer:   "accounts",
		Audience: ExecutionWorkContextAudience,
		Verifier: workContextVerifierFunc(func(
			_ context.Context,
			_ codefly.WorkContextToken,
			expected codefly.WorkContextExpectations,
		) (*basev0.WorkContextV1, error) {
			verified = true
			if expected.Issuer != "accounts" ||
				expected.Audience != ExecutionWorkContextAudience {
				t.Fatalf("expectations = %+v", expected)
			}
			return proto.Clone(claims).(*basev0.WorkContextV1), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := authority.Verify(t.Context(), codefly.WorkContextToken{}, Admission{
		ProducerID: "codefly.execution",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !verified || !proto.Equal(got, claims) {
		t.Fatalf("verified=%v claims=%+v", verified, got)
	}
}

func TestWorkContextAuthorityRejectsWildcardOtherProducerAndVerifierFailure(t *testing.T) {
	cases := []struct {
		name        string
		resourceIDs []string
		verifyErr   error
	}{
		{name: "wildcard"},
		{name: "other producer", resourceIDs: []string{"other.execution"}},
		{name: "invalid token", verifyErr: codefly.ErrWorkContextInvalid},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			claims := testClaims()
			claims.AuthorityScopes = []*basev0.WorkScopeV1{{
				ResourceKind: "evidence",
				Actions:      []string{"append"},
				ResourceIds:  testCase.resourceIDs,
			}}
			authority, err := NewWorkContextAuthority(WorkContextAuthorityConfig{
				Issuer: "accounts", Audience: ExecutionWorkContextAudience,
				Verifier: workContextVerifierFunc(func(
					context.Context,
					codefly.WorkContextToken,
					codefly.WorkContextExpectations,
				) (*basev0.WorkContextV1, error) {
					if testCase.verifyErr != nil {
						return nil, testCase.verifyErr
					}
					return claims, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = authority.Verify(t.Context(), codefly.WorkContextToken{}, Admission{
				ProducerID: "codefly.execution",
			})
			if err == nil {
				t.Fatal("expected authority rejection")
			}
			if testCase.verifyErr != nil && !errors.Is(err, testCase.verifyErr) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
