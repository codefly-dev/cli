package signing

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore/pkg/oauthflow"
	"golang.org/x/oauth2"
)

// A local run delivers to a host on a local cluster through the same delivery a
// cell uses, so its documents are signed through the same keyless flow: an
// ephemeral key certified by Fulcio for an OIDC identity, recorded in Rekor,
// packaged as a bundle the host verifies offline. What differs is the identity
// and where its token comes from — a developer signs with their own OIDC
// identity, obtained interactively, where a release signs with the workflow's
// (cli#918, docs/solution-host-bindings.md "Local runs"). There is no local
// trust mode: the local host's allowlist names the developer, a deployed host's
// names only its release workflow, and the ordinary allowlist check refuses
// each where it does not belong.

const (
	// SigstoreOAuthIssuer is the OIDC issuer of the Sigstore public-good
	// instance's federated login (GitHub, Google or Microsoft behind it), the
	// issuer whose tokens the public-good Fulcio certifies for a person.
	SigstoreOAuthIssuer = "https://oauth2.sigstore.dev/auth"
	// sigstoreClientID is the public client the public-good login knows.
	sigstoreClientID = "sigstore"

	// envSigstoreIDToken carries an OIDC token the developer obtained
	// themselves, for a terminal that cannot open a browser — the same
	// variable cosign reads. It is a token, never a key: Fulcio still
	// certifies whatever identity it names, and the developer policy still
	// refuses one the local host does not list.
	envSigstoreIDToken = "SIGSTORE_ID_TOKEN" //nolint:gosec // G101: the NAME of the variable a developer sets, never a credential value

	// tokenReuseMargin is how long before its expiry a token stops being
	// reused for another signature: one publish signs several documents, and
	// a browser round trip per document is not a flow anyone would run.
	tokenReuseMargin = 30 * time.Second
)

// ErrDeveloperNotAdmitted reports a developer token whose identity the
// environment's host.developers does not list: nothing is signed under it,
// because the local host would refuse the carrier, far from the cause.
var ErrDeveloperNotAdmitted = errors.New("the developer identity is not one the local host admits")

// IDTokenSource obtains an OIDC token for Fulcio to certify. It is the one
// thing a developer signer and a release signer differ in.
type IDTokenSource func(ctx context.Context) (string, error)

// DeveloperIdentity is one developer a local host's allowlist names: the OIDC
// issuer the certificate carries and its subject alternative name (the
// verified address).
type DeveloperIdentity struct {
	Issuer  string
	Subject string
}

// DeveloperPolicy is the set of developers a local host admits, as the
// composition states it in the k3d environment's host.developers.
type DeveloperPolicy struct {
	Developers []DeveloperIdentity
}

func (policy *DeveloperPolicy) validate() error {
	if policy == nil || len(policy.Developers) == 0 {
		return fmt.Errorf("%w: the developer policy names no developer; a local host admits only the identities its local profile lists (host.developers)", ErrPolicy)
	}
	for _, developer := range policy.Developers {
		if strings.TrimSpace(developer.Issuer) == "" || strings.TrimSpace(developer.Subject) == "" {
			return fmt.Errorf("%w: a developer identity needs both an issuer and a subject, got %q at %q", ErrPolicy, developer.Subject, developer.Issuer)
		}
	}
	return nil
}

// subjects lists the policy's subjects, sorted, for messages.
func (policy *DeveloperPolicy) subjects() string {
	names := make([]string, 0, len(policy.Developers))
	for _, developer := range policy.Developers {
		names = append(names, developer.Subject+" ("+developer.Issuer+")")
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// admitsSubject reports whether a token naming subject could be certified as
// one of the policy's developers. The issuer is not knowable from the token:
// the public-good login's token is issued by the federating login, and the
// certificate names the upstream provider — so the issuer is held after
// signing, by the check, and only the subject before.
func (policy *DeveloperPolicy) admitsSubject(subject string) bool {
	for _, developer := range policy.Developers {
		if developer.Subject == subject {
			return true
		}
	}
	return false
}

// DeveloperCheckUnder builds the check a local publish holds every carrier
// to — a fresh signature right after it is made, and a carrier delivered
// earlier before it is reused: verified offline, as the local host will
// verify it, against the public-good trusted root (fetched through TUF on
// first use), under an identity the policy lists. One check for both,
// because a developer identity has no ref that moves: the identity that may
// sign is exactly the identity that may be reused.
func DeveloperCheckUnder(policy *DeveloperPolicy) (SelfCheck, error) {
	return developerCheck(policy, PublicGoodTrustedRoot)
}

func developerCheck(policy *DeveloperPolicy, trustedRoot func(context.Context) (*root.TrustedRoot, error)) (SelfCheck, error) {
	if err := policy.validate(); err != nil {
		return nil, err
	}
	policies := make([]Policy, 0, len(policy.Developers))
	for _, developer := range policy.Developers {
		policies = append(policies, Policy{Issuer: developer.Issuer, SubjectPattern: "^" + regexp.QuoteMeta(developer.Subject) + "$"})
	}
	described := policy.subjects()
	var once sync.Once
	var trusted *root.TrustedRoot
	var fetchErr error
	return func(ctx context.Context, bundle, payload []byte) error {
		once.Do(func() { trusted, fetchErr = trustedRoot(ctx) })
		if fetchErr != nil {
			return fetchErr
		}
		var first error
		for _, candidate := range policies {
			_, err := Verify(bundle, payload, trusted, candidate)
			if err == nil {
				return nil
			}
			// A bundle carrying no transparency evidence fails every
			// identity the same way, and is reported by its own name.
			if errors.Is(err, ErrNoTransparency) {
				return fmt.Errorf("the carrier does not verify as the local host would verify it: %w", err)
			}
			if first == nil {
				first = err
			}
		}
		return fmt.Errorf("the carrier does not verify as the local host would verify it (a developer the environment's host.developers lists: %s): %w", described, first)
	}, nil
}

// NewDeveloper returns the keyless signer a local run signs with: the same
// Fulcio and Rekor flow as a release (opts as for NewKeyless), with an ID
// token from source instead of the workflow's, held to the policy BEFORE
// anything is signed — a token naming a developer the local host does not
// list is refused here, not after a certificate is issued and logged for it.
// The token is reused across the documents of one publish until shortly
// before it expires. A nil source means DeveloperIDToken(nil).
func NewDeveloper(policy *DeveloperPolicy, source IDTokenSource, opts KeylessOptions) (Signer, error) {
	if err := policy.validate(); err != nil {
		return nil, err
	}
	if source == nil {
		source = DeveloperIDToken(nil, policy)
	}
	opts.IDToken = admittedToken(policy, reusedToken(source, time.Now))
	return NewKeyless(opts), nil
}

// admittedToken holds every token the source gives to the policy's subjects.
func admittedToken(policy *DeveloperPolicy, source IDTokenSource) IDTokenSource {
	return func(ctx context.Context) (string, error) {
		token, err := source(ctx)
		if err != nil {
			return "", err
		}
		claims, err := tokenClaimsOf(token)
		if err != nil {
			return "", err
		}
		subject, err := claims.subject()
		if err != nil {
			return "", err
		}
		if !policy.admitsSubject(subject) {
			return "", fmt.Errorf("%w: the token names %s, and the environment's host.developers lists %s; sign in as one of them, or have the local host's profile and host.developers name you", ErrDeveloperNotAdmitted, subject, policy.subjects())
		}
		return token, nil
	}
}

// reusedToken obtains a token once and hands it out again until it is within
// tokenReuseMargin of its expiry (a token with no readable expiry is used
// once).
func reusedToken(source IDTokenSource, now func() time.Time) IDTokenSource {
	var mu sync.Mutex
	var cached string
	var expires time.Time
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if cached != "" && now().Add(tokenReuseMargin).Before(expires) {
			return cached, nil
		}
		token, err := source(ctx)
		if err != nil {
			return "", err
		}
		cached, expires = "", time.Time{}
		if claims, claimsErr := tokenClaimsOf(token); claimsErr == nil && claims.Expiry > 0 {
			cached, expires = token, time.Unix(claims.Expiry, 0)
		}
		return token, nil
	}
}

// tokenClaims are the claims a developer token is read for. Nothing here is
// verified — Fulcio verifies the token; this only decides whether to send it.
type tokenClaims struct {
	Subject  string `json:"sub"`
	Email    string `json:"email"`
	Verified any    `json:"email_verified"`
	Expiry   int64  `json:"exp"`
}

// subject is the identity Fulcio certifies a person's token for: the verified
// email when the token carries one, the subject otherwise.
func (claims *tokenClaims) subject() (string, error) {
	if claims.Email != "" {
		switch verified := claims.Verified.(type) {
		case bool:
			if verified {
				return claims.Email, nil
			}
		case string:
			if strings.EqualFold(verified, "true") {
				return claims.Email, nil
			}
		}
		return "", fmt.Errorf("the developer token's email %s is not verified by its identity provider, and Fulcio certifies only a verified one", claims.Email)
	}
	if claims.Subject == "" {
		return "", errors.New("the developer token names no subject")
	}
	return claims.Subject, nil
}

func tokenClaimsOf(token string) (*tokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("the developer token is not a JWT (header.payload.signature)")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, fmt.Errorf("the developer token's payload is not base64url: %w", err)
	}
	var claims tokenClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, fmt.Errorf("the developer token's payload is not JSON: %w", err)
	}
	return &claims, nil
}

// DeveloperIDToken is where a developer's token comes from: SIGSTORE_ID_TOKEN
// when the developer set it (a terminal with no browser), otherwise the
// interactive login of the Sigstore public-good instance, which opens the
// browser — preselecting the identity provider when every developer the
// policy lists is certified by the same one. lookup nil = os.LookupEnv.
func DeveloperIDToken(lookup func(string) (string, bool), policy *DeveloperPolicy) IDTokenSource {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if token, _ := lookup(envSigstoreIDToken); strings.TrimSpace(token) != "" {
		return func(context.Context) (string, error) { return strings.TrimSpace(token), nil }
	}
	return InteractiveIDToken(SigstoreOAuthIssuer, sigstoreClientID, connectorFor(policy))
}

// connectorFor is the upstream provider to preselect on the public-good
// login, or "" to let the developer choose: only when every listed developer
// is certified by the same provider the login federates.
func connectorFor(policy *DeveloperPolicy) string {
	if policy == nil {
		return ""
	}
	issuers := map[string]bool{}
	for _, developer := range policy.Developers {
		issuers[developer.Issuer] = true
	}
	if len(issuers) != 1 {
		return ""
	}
	for issuer := range issuers {
		switch issuer {
		case oauthflow.PublicInstanceGithubAuthSubURL, oauthflow.PublicInstanceGoogleAuthSubURL, oauthflow.PublicInstanceMicrosoftAuthSubURL:
			return issuer
		}
	}
	return ""
}

// InteractiveIDToken obtains a token through the issuer's browser login (an
// authorization-code flow with PKCE, redirected to a listener on loopback),
// the way cosign signs for a person. connector, when set, preselects the
// upstream identity provider of a federating issuer.
func InteractiveIDToken(issuer, clientID, connector string) IDTokenSource {
	return func(context.Context) (string, error) {
		getter := &oauthflow.InteractiveIDTokenGetter{HTMLPage: oauthflow.DefaultIDTokenGetter.HTMLPage}
		if connector != "" {
			getter.ExtraAuthURLParams = []oauth2.AuthCodeOption{oauthflow.ConnectorIDOpt(connector)}
		}
		token, err := oauthflow.OIDConnect(issuer, clientID, "", "", getter)
		if err != nil {
			return "", fmt.Errorf("signing in at %s to sign as a developer: %w", issuer, err)
		}
		return token.RawString, nil
	}
}
