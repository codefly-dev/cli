package signing

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/verify"
)

// MediaTypeBundle is the media type of a Sigstore bundle as this package writes and reads it.
const MediaTypeBundle = "application/vnd.dev.sigstore.bundle.v0.3+json"

// GitHubActionsIssuer is the OIDC issuer of a GitHub Actions workflow identity.
const GitHubActionsIssuer = "https://token.actions.githubusercontent.com"

// ErrNoIdentity is returned by a Signer that has no signing identity: this process is not the
// release workflow. Delivery documents are signed only by the owning repository's release
// workflow, under its GitHub Actions OIDC identity; there is no key an operator could supply.
var ErrNoIdentity = errors.New("no signing identity: delivery documents are signed only by the owning repository's release workflow, under its GitHub Actions OIDC identity, never by a key and never from this process")

// ErrSignature reports that a bundle does not verify for the given document under the given
// policy and trusted root: the signature, the identity, the transparency-log evidence or the
// bundle itself is not what the policy admits. The wrapped cause says which.
var ErrSignature = errors.New("delivery document signature verification failed")

// ErrNoTransparency reports a bundle that carries no transparency-log evidence at all — no
// inclusion promise and no inclusion proof — so nothing in it can establish, offline, that the
// signature was ever logged. That is a signer configured without a transparency log: a
// misconfiguration to fix at the signer, never a bad signature to investigate, and the two
// reach an operator looking identical unless they are named apart. So it is distinct from
// ErrSignature, as ErrPolicy is; a signed timestamp does not stand in, since it establishes
// when the signature was made and not that it was logged. The host refuses the same case by
// the same name, so a refusal reads the same wherever it is seen.
var ErrNoTransparency = errors.New("the bundle carries no transparency-log evidence (no inclusion promise or proof): the signer was not configured with a transparency log, which is fixed at the signer, not a signature that fails")

// ErrBundle reports bytes that are not a Sigstore bundle this package can read. Verify wraps it
// inside ErrSignature; ReadIdentity returns it on its own.
var ErrBundle = errors.New("not a readable Sigstore bundle")

// ErrPolicy reports a verification policy that could never admit a signer, such as an empty
// issuer or subject pattern: a configuration error, distinct from a signature that fails.
var ErrPolicy = errors.New("invalid signing policy")

// Signer signs the exact bytes it is given and returns a Sigstore bundle (JSON, MediaTypeBundle).
type Signer interface {
	Sign(ctx context.Context, payload []byte) ([]byte, error)
}

// Identity is who signed: the OIDC issuer and the certificate's subject alternative name (for
// GitHub Actions a URI such as
// https://github.com/<owner>/<repo>/.github/workflows/<file>@refs/tags/v1.2.3).
type Identity struct {
	Issuer  string
	Subject string
}

// Policy is the allowlist a verifier pins: exactly one issuer, and a regular expression the
// subject must match in full. Never a key.
//
// The pattern is anchored by Verify whether or not it carries ^ and $: a partial match would let
// a pattern meant for one tag admit a subject that merely contains it.
type Policy struct {
	Issuer         string
	SubjectPattern string
}

// GitHubActionsPolicy builds the policy for one repository's workflow: repository is
// "<owner>/<repo>", workflowPath is the path inside the repository
// (".github/workflows/release.yaml"), refPattern is a regular expression over the git ref
// ("refs/tags/v.*"). The subject pattern is
// ^https://github\.com/<repository>/<workflowPath>@<refPattern>$ with the literal parts quoted.
func GitHubActionsPolicy(repository, workflowPath, refPattern string) (Policy, error) {
	owner, name, found := strings.Cut(repository, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return Policy{}, fmt.Errorf("%w: repository %q must be \"<owner>/<repo>\"", ErrPolicy, repository)
	}
	workflowPath = strings.TrimPrefix(workflowPath, "/")
	if workflowPath == "" {
		return Policy{}, fmt.Errorf("%w: the workflow path is empty; expected the path inside the repository, such as .github/workflows/release.yaml", ErrPolicy)
	}
	if refPattern == "" {
		return Policy{}, fmt.Errorf("%w: the ref pattern is empty; expected a regular expression over the git ref, such as refs/tags/v.*", ErrPolicy)
	}
	pattern := "^" + regexp.QuoteMeta("https://github.com/"+repository+"/"+workflowPath) + "@" + refPattern + "$"
	if _, err := regexp.Compile(pattern); err != nil {
		return Policy{}, fmt.Errorf("%w: ref pattern %q: %w", ErrPolicy, refPattern, err)
	}
	return Policy{Issuer: GitHubActionsIssuer, SubjectPattern: pattern}, nil
}

// certificateIdentity turns the policy into the certificate identity sigstore-go enforces, or
// explains (ErrPolicy) why the policy could never admit anyone.
func (p Policy) certificateIdentity() (verify.CertificateIdentity, error) {
	if strings.TrimSpace(p.Issuer) == "" {
		return verify.CertificateIdentity{}, fmt.Errorf("%w: the issuer is empty; pin the OIDC issuer of the release workflow (for GitHub Actions %s)", ErrPolicy, GitHubActionsIssuer)
	}
	if strings.TrimSpace(p.SubjectPattern) == "" {
		return verify.CertificateIdentity{}, fmt.Errorf("%w: the subject pattern is empty; pin the release workflow's identity (for GitHub Actions use GitHubActionsPolicy)", ErrPolicy)
	}
	anchored := anchor(p.SubjectPattern)
	if _, err := regexp.Compile(anchored); err != nil {
		return verify.CertificateIdentity{}, fmt.Errorf("%w: subject pattern %q: %w", ErrPolicy, p.SubjectPattern, err)
	}
	identity, err := verify.NewShortCertificateIdentity(p.Issuer, "", "", anchored)
	if err != nil {
		return verify.CertificateIdentity{}, fmt.Errorf("%w: %w", ErrPolicy, err)
	}
	return identity, nil
}

// anchor makes pattern match the whole subject. sigstore-go's matcher is unanchored, and a
// partial match would let "…@refs/tags/v1" admit "…@refs/tags/v1-anything".
func anchor(pattern string) string {
	return "^(?:" + pattern + ")$"
}

// Unavailable is the Signer of a process with no signing identity. Sign returns ErrNoIdentity
// wrapped with a one-line reason (e.g. "ACTIONS_ID_TOKEN_REQUEST_URL is not set: not running in
// a GitHub Actions job").
type Unavailable struct{ Reason string }

// Sign refuses: there is no identity to sign under, and no key to fall back to.
func (u Unavailable) Sign(_ context.Context, _ []byte) ([]byte, error) {
	reason := u.Reason
	if reason == "" {
		reason = "this process has no CI OIDC identity"
	}
	return nil, fmt.Errorf("%s: %w", reason, ErrNoIdentity)
}
