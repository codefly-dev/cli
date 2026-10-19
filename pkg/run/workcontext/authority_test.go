package workcontext

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fetch reads the key set the authority serves over its admitted loopback hop.
func fetch(t *testing.T, authority *Authority) map[string]any {
	t.Helper()
	response, err := http.Get(authority.KeySetURL())
	if err != nil {
		t.Fatalf("the key set is not reachable at the URL the run hands out: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the key set answered %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("the key set is not JSON: %v", err)
	}
	return document
}

// The verifier admits exactly one shape: an OKP/Ed25519 signing key whose `x`
// is raw-URL base64 of a 32-byte public key. Emitting a padded encoding, a
// different curve or a JWK alg the parser rejects would leave the run serving a
// document no service can load, which is the failure this guards.
func TestServedKeySetIsTheShapeAVerifierAdmits(t *testing.T) {
	authority, err := Start("run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = authority.Close() }()

	keys, ok := fetch(t, authority)["keys"].([]any)
	if !ok || len(keys) != 1 {
		t.Fatalf("the key set must carry exactly one key, got %v", keys)
	}
	key, ok := keys[0].(map[string]any)
	if !ok {
		t.Fatalf("the key is not an object")
	}
	for field, want := range map[string]string{"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig"} {
		if key[field] != want {
			t.Errorf("%s is %v, and a verifier admits only %q", field, key[field], want)
		}
	}
	if kid, _ := key["kid"].(string); kid == "" {
		t.Error("the key carries no key id, so a token cannot name which key signed it")
	}
	encoded, _ := key["x"].(string)
	if strings.ContainsAny(encoded, "=+/") {
		t.Errorf("x must be raw-URL base64, got %q", encoded)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("x does not decode as raw-URL base64: %v", err)
	}
	if len(decoded) != ed25519.PublicKeySize {
		t.Fatalf("x is %d bytes, and an Ed25519 public key is %d", len(decoded), ed25519.PublicKeySize)
	}
}

// The published half must be the signing half's: a run that serves a key set
// unrelated to what it signs with refuses every capability it mints, which is
// indistinguishable at the far end from a key set that was never served.
func TestThePublishedKeyVerifiesWhatTheRunSigns(t *testing.T) {
	authority, err := Start("run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = authority.Close() }()

	key, _ := fetch(t, authority)["keys"].([]any)[0].(map[string]any)
	encoded, _ := key["x"].(string)
	published, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	keyID, private := authority.Signer()
	if keyID != key["kid"] {
		t.Errorf("the signer names key %q, the key set publishes %v", keyID, key["kid"])
	}
	message := []byte("a capability this run minted")
	if !ed25519.Verify(ed25519.PublicKey(published), message, ed25519.Sign(private, message)) {
		t.Fatal("the published key does not verify what the run signs")
	}
}

// Two runs must not accept each other's capabilities. A verifier pins the
// issuer, so the issuer is what separates them.
func TestEachRunIssuesUnderItsOwnIssuer(t *testing.T) {
	first, err := Start("run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := Start("run-2")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	if first.Issuer() == second.Issuer() {
		t.Fatalf("both runs issue under %q, so each would accept the other's capabilities", first.Issuer())
	}
	if !strings.Contains(first.Issuer(), "run-1") {
		t.Errorf("the issuer does not name the run: %q", first.Issuer())
	}
}

// A run with no identity cannot issue under one.
func TestARunWithoutAnIdentityServesNothing(t *testing.T) {
	if _, err := Start(""); err == nil {
		t.Fatal("a run with no identity was allowed to issue capabilities")
	}
}
