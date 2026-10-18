package signing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/solutionhost"
	"golang.org/x/oauth2"
)

const kmsTestVersion = "projects/example-platform/locations/us-east1/keyRings/delivery/cryptoKeys/delivery/cryptoKeyVersions/3"

func kmsLookup(endpoint string, transit ...string) func(string) (string, bool) {
	values := map[string]string{"CODEFLY_RELEASE_KMS_KEY": kmsTestVersion, "CODEFLY_RELEASE_KMS_ENDPOINT": endpoint}
	for _, name := range transit {
		values[name] = "set"
	}
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// fakeKMS is the two Cloud KMS operations the custody uses, over a P-256 key
// it holds, plus a token endpoint for the ambient-credential test.
type fakeKMS struct {
	t         *testing.T
	key       *ecdsa.PrivateKey
	algorithm string
	pem       string
	calls     int
	fail      string
}

func newFakeKMS(t *testing.T) *fakeKMS {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	return &fakeKMS{t: t, key: key, algorithm: kmsAlgorithmP256, pem: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}
}

func (f *fakeKMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls++
	raw, _ := io.ReadAll(r.Body)
	if r.URL.Path == "/token" {
		fmt.Fprint(w, `{"access_token":"private-test-token","token_type":"Bearer","expires_in":3600}`)
		return
	}
	if r.Header.Get("Authorization") != "Bearer private-test-token" {
		f.t.Error("missing bearer token")
	}
	if strings.Contains(r.URL.String(), "private-test-token") || strings.Contains(string(raw), "private-test-token") {
		f.t.Error("token outside the Authorization header")
	}
	switch f.fail {
	case "status":
		w.WriteHeader(403)
		fmt.Fprint(w, "private-test-token")
		return
	case "malformed":
		fmt.Fprint(w, "private-test-token")
		return
	}
	switch r.URL.Path {
	case "/v1/" + kmsTestVersion + "/publicKey":
		if r.Method != http.MethodGet || len(raw) != 0 {
			f.t.Error("wrong public key request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"pem": f.pem, "algorithm": f.algorithm, "pemCrc32c": crc32c([]byte(f.pem)), "name": kmsTestVersion})
	case "/v1/" + kmsTestVersion + ":asymmetricSign":
		if r.Method != http.MethodPost {
			f.t.Error("wrong sign method")
		}
		var body struct {
			Digest       map[string]string `json:"digest"`
			DigestCRC32C string            `json:"digestCrc32c"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || len(body.Digest) != 1 {
			f.t.Error("wrong sign request")
			return
		}
		digest, err := base64.StdEncoding.DecodeString(body.Digest["sha256"])
		if err != nil || len(digest) != 32 || !crc32cMatches(body.DigestCRC32C, digest) {
			f.t.Error("the request does not carry a checked SHA-256 digest")
			return
		}
		signature, err := ecdsa.SignASN1(rand.Reader, f.key, digest)
		if err != nil {
			f.t.Error(err)
			return
		}
		name, verified := kmsTestVersion, true
		switch f.fail {
		case "version":
			name = strings.TrimSuffix(kmsTestVersion, "3") + "4"
		case "unverified digest":
			verified = false
		case "corrupt signature":
			signature[len(signature)-1]++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "signature": base64.StdEncoding.EncodeToString(signature), "signatureCrc32c": crc32c(signature), "verifiedDigestCrc32c": verified})
	default:
		f.t.Errorf("unexpected request %s", r.URL.Path)
		w.WriteHeader(404)
	}
}

var kmsTestCredential = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "private-test-token"})

func TestCloudKMSSignsAVerifiableP256Envelope(t *testing.T) {
	kms := newFakeKMS(t)
	server := httptest.NewServer(kms)
	defer server.Close()
	signer, err := NewCloudKMS(kmsLookup(server.URL), kmsTestCredential)
	if err != nil {
		t.Fatal(err)
	}
	if signer.KeyID() != "delivery-v3" {
		t.Fatalf("derived key id %q", signer.KeyID())
	}
	if kms.calls != 1 {
		t.Fatalf("%d requests before signing, want the public key read once", kms.calls)
	}
	// The host's own pin check over the fixture's ownership domain, then
	// core's verifier over the envelope.
	check, err := KeyCheck(signer, solutionhost.KeyKindRelease, "alpha", "delivery-v3")
	if err != nil {
		t.Fatal(err)
	}
	payload := testPayload(t)
	envelope, err := SignEnvelope(payload, signer)
	if err != nil {
		t.Fatal(err)
	}
	if err = check(context.Background(), envelope, payload); err != nil {
		t.Fatal(err)
	}
	if kms.calls != 2 {
		t.Fatalf("%d requests", kms.calls)
	}
	if _, err = KeyCheck(signer, solutionhost.KeyKindRelease, "alpha", "delivery-v2"); err == nil || !strings.Contains(err.Error(), "delivery-v3") {
		t.Fatalf("a pin for another version was admitted: %v", err)
	}
}

func TestCloudKMSRefusesAnyOtherAlgorithm(t *testing.T) {
	for _, algorithm := range []string{"EC_SIGN_P384_SHA384", "RSA_SIGN_PKCS1_2048_SHA256", "EC_SIGN_SECP256K1_SHA256", "", "private-test-token"} {
		kms := newFakeKMS(t)
		kms.algorithm = algorithm
		server := httptest.NewServer(kms)
		_, err := NewCloudKMS(kmsLookup(server.URL), kmsTestCredential)
		server.Close()
		if err == nil || !strings.Contains(err.Error(), kmsAlgorithmP256) || strings.Contains(err.Error(), "private-test-token") {
			t.Fatalf("%q: %v", algorithm, err)
		}
		if algorithm != "" && algorithm != "private-test-token" && !strings.Contains(err.Error(), algorithm) {
			t.Fatalf("the refusal does not name the key's algorithm: %v", err)
		}
	}
	// A P-256 claim over another curve's key is refused as well.
	kms := newFakeKMS(t)
	other, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&other.PublicKey)
	kms.pem = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	server := httptest.NewServer(kms)
	defer server.Close()
	if _, err := NewCloudKMS(kmsLookup(server.URL), kmsTestCredential); err == nil {
		t.Fatal("a P-384 key was admitted as P-256")
	}
}

func TestCloudKMSRefusalsNeverExposeToken(t *testing.T) {
	for _, failure := range []string{"status", "malformed", "version", "unverified digest", "corrupt signature", "redirect"} {
		t.Run(failure, func(t *testing.T) {
			targetCalls := 0
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls++ }))
			defer target.Close()
			kms := newFakeKMS(t)
			kms.fail = failure
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if failure == "redirect" {
					http.Redirect(w, r, target.URL, 302)
					return
				}
				kms.ServeHTTP(w, r)
			}))
			defer server.Close()
			signer, err := NewCloudKMS(kmsLookup(server.URL), kmsTestCredential)
			if err == nil {
				_, err = SignEnvelope(testPayload(t), signer)
			}
			if err == nil || strings.Contains(err.Error(), "private-test-token") {
				t.Fatalf("unsafe refusal: %v", err)
			}
			if targetCalls != 0 {
				t.Fatal("redirect followed")
			}
		})
	}
}

func TestCloudKMSRejectsUnsafeConfiguration(t *testing.T) {
	for _, endpoint := range []string{"http://example.test", "http://192.0.2.1", "ftp://localhost", "https://private-test-token@example.test", "https://example.test?token=private-test-token"} {
		if _, err := NewCloudKMS(kmsLookup(endpoint), kmsTestCredential); err == nil || strings.Contains(err.Error(), "private-test-token") {
			t.Fatalf("invalid endpoint %s: %v", endpoint, err)
		}
	}
	for _, resource := range []string{
		"delivery",
		"projects/p/locations/l/keyRings/r/cryptoKeys/delivery",
		"projects/p/locations/l/keyRings/r/cryptoKeys/delivery/cryptoKeyVersions/0",
		"projects/p/locations/l/keyRings/r/cryptoKeys/delivery/cryptoKeyVersions/03",
		"projects/p/locations/l/keyRings/r/cryptoKeys/../cryptoKeyVersions/3",
		"projects/p/locations/l/keyRings/r/cryptoKeys/deli very/cryptoKeyVersions/3",
		"projects/p/locations/l/keyRings/r/cryptoKeys/delivery/cryptoKeyVersions/3/publicKey",
	} {
		lookup := func(key string) (string, bool) {
			if key == "CODEFLY_RELEASE_KMS_KEY" {
				return resource, true
			}
			return "", false
		}
		if _, err := NewCloudKMS(lookup, kmsTestCredential); err == nil || !strings.Contains(err.Error(), kmsVersionShape) {
			t.Fatalf("resource %q: %v", resource, err)
		}
	}
}

func TestReleaseCustodySelection(t *testing.T) {
	empty := func(string) (string, bool) { return "", false }
	if _, err := FromEnvironment(empty).Sign(nil); err == nil || !strings.Contains(err.Error(), "CODEFLY_RELEASE_KMS_KEY") || !strings.Contains(err.Error(), "CODEFLY_RELEASE_TRANSIT_KEY") {
		t.Fatalf("the no-identity refusal does not name both custodies: %v", err)
	}
	for _, transit := range [][]string{{"VAULT_ADDR"}, {"CODEFLY_RELEASE_TRANSIT_KEY"}, {"VAULT_ADDR", "VAULT_TOKEN", "CODEFLY_RELEASE_TRANSIT_KEY"}} {
		_, err := FromEnvironment(kmsLookup("https://example.test", transit...)).Sign(nil)
		if err == nil || !strings.Contains(err.Error(), "CODEFLY_RELEASE_KMS_KEY") || !strings.Contains(err.Error(), strings.Join(transit, ", ")) {
			t.Fatalf("both custodies configured (%v) was not refused naming both: %v", transit, err)
		}
	}
	// Transit alone still selects Transit: its address is checked before any request.
	if _, err := FromEnvironment(vaultLookup("http://example.test")).Sign(nil); err == nil || !strings.Contains(err.Error(), "VAULT_ADDR requires https") {
		t.Fatalf("transit not selected: %v", err)
	}
}

// With only CODEFLY_RELEASE_KMS_KEY set, publish signs through the ambient
// Google credential: a service-account credential file exchanged at its own
// token endpoint, then the bearer token at Cloud KMS.
func TestCloudKMSUsesTheAmbientGoogleCredential(t *testing.T) {
	kms := newFakeKMS(t)
	server := httptest.NewServer(kms)
	defer server.Close()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)})
	credential, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "example-platform", "private_key_id": "1", "private_key": string(privatePEM),
		"client_email": "release@example-platform.iam.gserviceaccount.com", "token_uri": server.URL + "/token",
	})
	path := filepath.Join(t.TempDir(), "credential.json")
	if err = os.WriteFile(path, credential, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	signer := FromEnvironment(kmsLookup(server.URL))
	if _, err = SignEnvelope(testPayload(t), signer); err != nil {
		t.Fatal(err)
	}
	if signer.KeyID() != "delivery-v3" || kms.calls != 3 {
		t.Fatalf("key %s after %d requests, want token + public key + sign", signer.KeyID(), kms.calls)
	}
}
