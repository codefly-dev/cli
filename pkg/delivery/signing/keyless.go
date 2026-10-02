package signing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/sign"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	// DefaultFulcioURL is the certificate authority of the Sigstore public-good instance.
	DefaultFulcioURL = "https://fulcio.sigstore.dev"
	// DefaultRekorURL is the transparency log of the Sigstore public-good instance.
	DefaultRekorURL = "https://rekor.sigstore.dev"

	// fulcioAudience is the audience Fulcio requires of the OIDC token it certifies.
	fulcioAudience = "sigstore"

	// GitHub Actions exposes the job's OIDC endpoint through these two variables, and only when
	// the job grants `id-token: write`. Their presence is what makes this process a signer.
	envActionsIDRequestURL  = "ACTIONS_ID_TOKEN_REQUEST_URL"
	envActionsIDRequestAuth = "ACTIONS_ID_TOKEN_REQUEST_TOKEN"

	// networkRetries is how many times a 5xx or 429 from Fulcio, Rekor or the timestamp
	// authority is retried before a release fails.
	networkRetries = 2

	// maxTokenResponse bounds what is read from the token endpoint; a JWT is a few kilobytes.
	maxTokenResponse = 1 << 20
)

// KeylessOptions configures keyless signing. Zero values mean the Sigstore public-good instance
// (Fulcio https://fulcio.sigstore.dev, Rekor https://rekor.sigstore.dev); TimestampURL empty means
// no RFC 3161 timestamp. IDToken obtains the OIDC token; nil means GitHubActionsIDToken.
type KeylessOptions struct {
	FulcioURL    string
	RekorURL     string
	TimestampURL string
	IDToken      func(ctx context.Context) (string, error)
	HTTPClient   *http.Client // nil = http.DefaultClient, used for the ID token request
}

// Keyless signs with an ephemeral ECDSA P-256 key certified by Fulcio for the OIDC identity and
// records the signature in Rekor. The key is generated inside Sign and dropped when it returns;
// nothing persists but the bundle.
type Keyless struct {
	options KeylessOptions
}

// NewKeyless returns a keyless signer, filling every zero option with the public-good default.
func NewKeyless(opts KeylessOptions) *Keyless {
	if opts.FulcioURL == "" {
		opts.FulcioURL = DefaultFulcioURL
	}
	if opts.RekorURL == "" {
		opts.RekorURL = DefaultRekorURL
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.IDToken == nil {
		client := opts.HTTPClient
		opts.IDToken = func(ctx context.Context) (string, error) {
			return GitHubActionsIDToken(ctx, client)
		}
	}
	return &Keyless{options: opts}
}

// Sign obtains the OIDC token, has Fulcio certify a fresh ephemeral key for it, signs payload,
// records the signature in Rekor and returns the bundle as JSON (MediaTypeBundle). Nothing is
// contacted when no token can be obtained: the error then wraps whatever IDToken returned, for
// GitHubActionsIDToken an ErrNoIdentity.
func (k *Keyless) Sign(ctx context.Context, payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return nil, errors.New("signing: refusing to sign an empty delivery document")
	}
	token, err := k.options.IDToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("obtaining the workflow's OIDC token: %w", err)
	}
	if token == "" {
		return nil, fmt.Errorf("obtaining the workflow's OIDC token: the token endpoint returned an empty token: %w", ErrNoIdentity)
	}

	keypair, err := sign.NewEphemeralKeypair(nil) // ECDSA P-256 with SHA-256, for this call only
	if err != nil {
		return nil, fmt.Errorf("generating the ephemeral signing key: %w", err)
	}

	options := sign.BundleOptions{
		CertificateProvider:        sign.NewFulcio(&sign.FulcioOptions{BaseURL: k.options.FulcioURL, Retries: networkRetries}),
		CertificateProviderOptions: &sign.CertificateProviderOptions{IDToken: token},
		TransparencyLogs:           []sign.Transparency{sign.NewRekor(&sign.RekorOptions{BaseURL: k.options.RekorURL, Retries: networkRetries})},
		Context:                    ctx,
	}
	if k.options.TimestampURL != "" {
		options.TimestampAuthorities = []*sign.TimestampAuthority{
			sign.NewTimestampAuthority(&sign.TimestampAuthorityOptions{URL: k.options.TimestampURL, Retries: networkRetries}),
		}
	}

	bundle, err := sign.Bundle(&sign.PlainData{Data: payload}, keypair, options)
	if err != nil {
		return nil, fmt.Errorf("signing with Sigstore (Fulcio %s, Rekor %s): %w", k.options.FulcioURL, k.options.RekorURL, err)
	}
	raw, err := protojson.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("encoding the Sigstore bundle: %w", err)
	}
	return raw, nil
}

// GitHubActionsIDToken requests the workflow's OIDC token for audience "sigstore" from the GitHub
// Actions token endpoint: GET $ACTIONS_ID_TOKEN_REQUEST_URL with audience=sigstore added to its
// query and the header "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN"; the response is
// JSON {"value": "<jwt>"}. It returns an error wrapping ErrNoIdentity when either variable is
// unset. A nil client means http.DefaultClient.
func GitHubActionsIDToken(ctx context.Context, client *http.Client) (string, error) {
	return gitHubActionsIDToken(ctx, client, os.LookupEnv)
}

// FromEnvironment returns the Signer this process may use: a Keyless signer with default options
// when both ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN are set, otherwise
// Unavailable with the reason. It reads the environment through the lookup function so tests
// need not mutate the process environment (lookup nil = os.LookupEnv).
func FromEnvironment(lookup func(string) (string, bool)) Signer {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if reason := missingIdentityReason(lookup); reason != "" {
		return &Unavailable{Reason: reason}
	}
	return NewKeyless(KeylessOptions{
		IDToken: func(ctx context.Context) (string, error) {
			return gitHubActionsIDToken(ctx, http.DefaultClient, lookup)
		},
	})
}

func gitHubActionsIDToken(ctx context.Context, client *http.Client, lookup func(string) (string, bool)) (string, error) {
	if reason := missingIdentityReason(lookup); reason != "" {
		return "", fmt.Errorf("%s: %w", reason, ErrNoIdentity)
	}
	endpoint, _ := lookup(envActionsIDRequestURL)
	bearer, _ := lookup(envActionsIDRequestAuth)
	return requestIDToken(ctx, client, endpoint, bearer)
}

// missingIdentityReason names what the environment lacks for this process to be a GitHub
// Actions job that can sign, or "" when it lacks nothing. An empty value counts as unset.
func missingIdentityReason(lookup func(string) (string, bool)) string {
	var missing []string
	for _, name := range []string{envActionsIDRequestURL, envActionsIDRequestAuth} {
		if value, _ := lookup(name); value == "" {
			missing = append(missing, name)
		}
	}
	switch len(missing) {
	case 0:
		return ""
	case 1:
		return missing[0] + " is not set: not running in a GitHub Actions job with id-token permission"
	default:
		return strings.Join(missing, " and ") + " are not set: not running in a GitHub Actions job"
	}
}

// requestIDToken performs the token request against the endpoint GitHub Actions handed the job.
func requestIDToken(ctx context.Context, client *http.Client, endpoint, bearer string) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("%s is not a URL: %w", envActionsIDRequestURL, err)
	}
	query := target.Query()
	query.Set("audience", fulcioAudience)
	target.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return "", fmt.Errorf("building the OIDC token request: %w", err)
	}
	request.Header.Set("Authorization", "bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("requesting the OIDC token from GitHub Actions: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponse))
	if err != nil {
		return "", fmt.Errorf("reading the OIDC token response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the GitHub Actions token endpoint answered %s: %s", response.Status, excerpt(body))
	}
	var answer struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", fmt.Errorf("decoding the OIDC token response: %w", err)
	}
	if answer.Value == "" {
		return "", errors.New("the GitHub Actions token endpoint returned no token")
	}
	return answer.Value, nil
}

// excerpt keeps an error body readable on one line.
func excerpt(body []byte) string {
	const limit = 200
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > limit {
		return text[:limit] + "…"
	}
	if text == "" {
		return "(empty body)"
	}
	return text
}
