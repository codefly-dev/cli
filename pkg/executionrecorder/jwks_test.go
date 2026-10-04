package executionrecorder

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/core/workcontext"
)

// jwksIssuer publishes a set of keys as a JWK Set and lets a test rotate it.
type jwksIssuer struct {
	mu       sync.Mutex
	keys     []jwk
	requests int
}

func (issuer *jwksIssuer) publish(keyID string, public ed25519.PublicKey) {
	issuer.mu.Lock()
	defer issuer.mu.Unlock()
	issuer.keys = append(issuer.keys, jwk{KeyType: "OKP", Curve: "Ed25519", Use: "sig", Alg: "EdDSA", KeyID: keyID, X: base64.RawURLEncoding.EncodeToString(public)})
}

func (issuer *jwksIssuer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	issuer.mu.Lock()
	defer issuer.mu.Unlock()
	issuer.requests++
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jwkSet{Keys: issuer.keys})
}

func (issuer *jwksIssuer) served() int {
	issuer.mu.Lock()
	defer issuer.mu.Unlock()
	return issuer.requests
}

// TestJWKSKeysFollowTheIssuersRotation: the keys come from the issuer's JWKS
// over TLS, are cached, and are refreshed when a capability names a key the
// set does not hold — which is how a rotation reaches the verifier. A key id
// the issuer never published is refused by core, naming the key.
func TestJWKSKeysFollowTheIssuersRotation(t *testing.T) {
	issuer := newTestIssuer(t)
	published := &jwksIssuer{}
	published.publish(issuer.keyID, issuer.public)
	server := httptest.NewTLSServer(published)
	t.Cleanup(server.Close)

	clock := issuer.now
	keys, err := NewJWKSKeys(server.URL+"/.well-known/work-context-jwks.json", JWKSOptions{
		HTTPClient: server.Client(), Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	authority, err := NewWorkContextAuthority(&WorkContextAuthorityConfig{
		Issuer: testIssuerName, Audience: ExecutionWorkContextAudience, Keys: keys,
		Revisions: issuer.revision, Seals: issuer.seals, Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	admission := Admission{ProducerID: "codefly.execution"}
	if _, err := authority.Verify(t.Context(), issuer.mint(t, evidenceScope("codefly.execution")), admission); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Verify(t.Context(), issuer.mint(t, evidenceScope("codefly.execution")), admission); err != nil {
		t.Fatal(err)
	}
	if published.served() != 1 {
		t.Fatalf("the JWKS was fetched %d times for two verifications under a held key", published.served())
	}

	// The issuer rotates: a new key signs, the old one is still published.
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer.authority.KeyID, issuer.authority.Key = "key-2", private
	rotated := issuer.mint(t, evidenceScope("codefly.execution"))
	// Not published yet, and the set was refreshed under a minute ago: the
	// capability is refused by the key it names, and the issuer is not asked
	// again.
	if _, err := authority.Verify(t.Context(), rotated, admission); !errors.Is(err, workcontext.ErrInvalid) || !strings.Contains(err.Error(), `"key-2"`) {
		t.Fatalf("unpublished key: err = %v", err)
	}
	if published.served() != 1 {
		t.Fatalf("an unknown key id refetched the JWKS %d times within the refresh interval", published.served())
	}
	published.publish("key-2", public)
	clock = clock.Add(2 * time.Minute)
	if _, err := authority.Verify(t.Context(), rotated, admission); err != nil {
		t.Fatalf("rotated key after refresh: %v", err)
	}
	if published.served() != 2 {
		t.Fatalf("the JWKS was fetched %d times, want 2 (one refresh for the unknown key)", published.served())
	}

	// The cache ages out and is fetched again on the next verification.
	clock = clock.Add(defaultJWKSCacheTTL)
	if _, err := authority.Verify(t.Context(), rotated, admission); err != nil {
		t.Fatal(err)
	}
	if published.served() != 3 {
		t.Fatalf("the JWKS was fetched %d times, want 3 (one for the aged cache)", published.served())
	}
}

func TestJWKSKeysRefuseWhatIsNotAnEd25519SigningSet(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	x := base64.RawURLEncoding.EncodeToString(public)
	for name, document := range map[string]string{
		"empty set":        `{"keys":[]}`,
		"RSA key":          `{"keys":[{"kty":"RSA","kid":"k","n":"AQAB","e":"AQAB"}]}`,
		"other curve":      `{"keys":[{"kty":"OKP","crv":"X25519","kid":"k","x":"` + x + `"}]}`,
		"encryption use":   `{"keys":[{"kty":"OKP","crv":"Ed25519","use":"enc","kid":"k","x":"` + x + `"}]}`,
		"no key id":        `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + x + `"}]}`,
		"short key":        `{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"k","x":"AAAA"}]}`,
		"duplicate key id": `{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"k","x":"` + x + `"},{"kty":"OKP","crv":"Ed25519","kid":"k","x":"` + x + `"}]}`,
		"not json":         `keys`,
	} {
		if _, err := parseJWKS([]byte(document)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	keys, err := parseJWKS([]byte(`{"keys":[{"kty":"OKP","crv":"Ed25519","use":"sig","alg":"EdDSA","kid":"k","x":"` + x + `"}]}`))
	if err != nil || !keys["k"].Equal(public) {
		t.Fatalf("keys=%v err=%v", keys, err)
	}
}

func TestJWKSKeysRequireHTTPSExceptOnLoopback(t *testing.T) {
	for _, url := range []string{"http://accounts.example.test/keys", "ftp://accounts.example.test/keys", "accounts.example.test/keys", "https:///keys"} {
		if _, err := NewJWKSKeys(url, JWKSOptions{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: err = %v", url, err)
		}
	}
	for _, url := range []string{"https://accounts.example.test/keys", "http://127.0.0.1:8080/keys", "http://localhost:8080/keys", "http://[::1]:8080/keys"} {
		if _, err := NewJWKSKeys(url, JWKSOptions{}); err != nil {
			t.Fatalf("%s: %v", url, err)
		}
	}
}

// TestJWKSKeysDoNotServeAnAgedCacheWhenTheIssuerIsUnreachable: a verifier that
// cannot confirm its keys are current refuses rather than trusts the set it
// held.
func TestJWKSKeysDoNotServeAnAgedCacheWhenTheIssuerIsUnreachable(t *testing.T) {
	issuer := newTestIssuer(t)
	published := &jwksIssuer{}
	published.publish(issuer.keyID, issuer.public)
	server := httptest.NewTLSServer(published)
	clock := issuer.now
	keys, err := NewJWKSKeys(server.URL, JWKSOptions{HTTPClient: server.Client(), Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Keys(t.Context(), issuer.keyID); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if _, err := keys.Keys(t.Context(), issuer.keyID); err != nil {
		t.Fatalf("a held key within the cache TTL needs no fetch: %v", err)
	}
	clock = clock.Add(defaultJWKSCacheTTL)
	if _, err := keys.Keys(t.Context(), issuer.keyID); err == nil || !strings.Contains(err.Error(), "fetch Work Context JWKS") {
		t.Fatalf("aged cache with the issuer unreachable: err = %v", err)
	}
}
