package executionrecorder

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// KeySource answers the issuer's Ed25519 verification keys by key id, as the
// issuer publishes them now. It is key DISTRIBUTION and nothing more: what a
// capability says under one of these keys is decided by core's verifier.
type KeySource interface {
	// Keys returns the keys held. keyID names the key the capability under
	// verification claims, so a source that can refresh does so when it does
	// not hold that key — a rotation reaches a verifier through the first
	// capability that names the new key. "" asks for whatever is held.
	Keys(ctx context.Context, keyID string) (map[string]ed25519.PublicKey, error)
}

// StaticKeys is a KeySource that holds a fixed set: tests, and an issuer whose
// keys are configured rather than published.
type StaticKeys map[string]ed25519.PublicKey

// Keys implements KeySource.
func (keys StaticKeys) Keys(context.Context, string) (map[string]ed25519.PublicKey, error) {
	return maps.Clone(keys), nil
}

// JWKSOptions tune a JWKSKeys source. Every zero value has a default.
type JWKSOptions struct {
	HTTPClient     *http.Client
	CacheTTL       time.Duration
	RequestTimeout time.Duration
	Now            func() time.Time
}

const (
	defaultJWKSCacheTTL       = 5 * time.Minute
	defaultJWKSRequestTimeout = 10 * time.Second
	// A capability naming a key the set does not hold refreshes the set, but
	// not more than once a minute: a stream of unknown key ids is not a reason
	// to hammer the issuer, and a rotation is not that frequent.
	jwksUnknownKeyRefreshInterval = time.Minute
	maxJWKSKeys                   = 16
	maxJWKSBytes                  = 64 << 10
	maxJWKSKeyIDBytes             = 256
)

// JWKSKeys fetches the issuer's keys from a JWK Set document (RFC 7517; the
// keys are OKP/Ed25519 per RFC 8037) over HTTPS, caches them, and refreshes
// when the cache ages out or a capability names a key it does not hold.
type JWKSKeys struct {
	url     string
	client  *http.Client
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time

	mu        sync.Mutex
	keys      map[string]ed25519.PublicKey
	fetched   time.Time
	refreshed time.Time
}

// NewJWKSKeys validates the URL and returns a source that fetches lazily; the
// issuer is not contacted here. HTTPS is required except for a loopback host,
// which is what a test issuer answers from.
func NewJWKSKeys(rawURL string, options JWKSOptions) (*JWKSKeys, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("%w: Work Context JWKS URL: %v", ErrInvalid, err)
	}
	host := parsed.Hostname()
	loopback := host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
	secure := parsed.Scheme == "https" || parsed.Scheme == "http" && loopback
	if host == "" || !secure {
		return nil, fmt.Errorf("%w: Work Context JWKS URL %q must be https (http only for a loopback host)", ErrInvalid, rawURL)
	}
	source := &JWKSKeys{
		url: parsed.String(), client: options.HTTPClient, ttl: options.CacheTTL,
		timeout: options.RequestTimeout, now: options.Now,
	}
	if source.client == nil {
		source.client = http.DefaultClient
	}
	if source.ttl <= 0 {
		source.ttl = defaultJWKSCacheTTL
	}
	if source.timeout <= 0 {
		source.timeout = defaultJWKSRequestTimeout
	}
	if source.now == nil {
		source.now = time.Now
	}
	return source, nil
}

// Keys implements KeySource. A fetch that fails leaves the previous set in
// place but does not serve it past its TTL: a verifier that cannot reach its
// issuer refuses rather than trusts keys it cannot confirm are current.
func (source *JWKSKeys) Keys(ctx context.Context, keyID string) (map[string]ed25519.PublicKey, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	now := source.now()
	_, held := source.keys[keyID]
	fresh := source.keys != nil && now.Sub(source.fetched) < source.ttl
	switch {
	case fresh && (keyID == "" || held):
		return maps.Clone(source.keys), nil
	case fresh && now.Sub(source.refreshed) < jwksUnknownKeyRefreshInterval:
		// An unknown key id, but the set was refreshed under a minute ago:
		// the key is unknown to the issuer too, as far as this verifier may
		// ask. Core refuses the capability by that key id.
		return maps.Clone(source.keys), nil
	}
	keys, err := source.fetch(ctx)
	if err != nil {
		return nil, err
	}
	source.keys, source.fetched, source.refreshed = keys, now, now
	return maps.Clone(keys), nil
}

func (source *JWKSKeys) fetch(ctx context.Context) (map[string]ed25519.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, source.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%w: Work Context JWKS request: %v", ErrInvalid, err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := source.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch Work Context JWKS: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch Work Context JWKS: %s answered %s", source.url, response.Status)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch Work Context JWKS: %w", err)
	}
	if len(payload) > maxJWKSBytes {
		return nil, fmt.Errorf("%w: Work Context JWKS exceeds %d bytes", ErrInvalid, maxJWKSBytes)
	}
	return parseJWKS(payload)
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	KeyType string `json:"kty"`
	Curve   string `json:"crv"`
	Use     string `json:"use"`
	Alg     string `json:"alg"`
	KeyID   string `json:"kid"`
	X       string `json:"x"`
}

// parseJWKS reads a JWK Set of Ed25519 signing keys. Anything else in the set
// refuses the whole document: a verifier does not pick the keys it likes out
// of a set it does not understand.
func parseJWKS(payload []byte) (map[string]ed25519.PublicKey, error) {
	var document jwkSet
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, fmt.Errorf("%w: decode Work Context JWKS: %v", ErrInvalid, err)
	}
	if len(document.Keys) == 0 || len(document.Keys) > maxJWKSKeys {
		return nil, fmt.Errorf("%w: Work Context JWKS must hold between 1 and %d keys", ErrInvalid, maxJWKSKeys)
	}
	keys := make(map[string]ed25519.PublicKey, len(document.Keys))
	for _, key := range document.Keys {
		if key.KeyType != "OKP" || key.Curve != "Ed25519" || key.Alg != "" && key.Alg != "EdDSA" || key.Use != "" && key.Use != "sig" {
			return nil, fmt.Errorf("%w: Work Context JWKS holds a key that is not an Ed25519 signing key", ErrInvalid)
		}
		if key.KeyID == "" || len(key.KeyID) > maxJWKSKeyIDBytes || strings.TrimSpace(key.KeyID) != key.KeyID {
			return nil, fmt.Errorf("%w: Work Context JWKS key id %q is empty, padded or longer than %d bytes", ErrInvalid, key.KeyID, maxJWKSKeyIDBytes)
		}
		decoded, err := base64.RawURLEncoding.DecodeString(key.X)
		if err != nil || len(decoded) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: Work Context JWKS key %q is not a %d-byte Ed25519 public key", ErrInvalid, key.KeyID, ed25519.PublicKeySize)
		}
		if _, duplicate := keys[key.KeyID]; duplicate {
			return nil, fmt.Errorf("%w: Work Context JWKS names key id %q twice", ErrInvalid, key.KeyID)
		}
		keys[key.KeyID] = ed25519.PublicKey(decoded)
	}
	return keys, nil
}
