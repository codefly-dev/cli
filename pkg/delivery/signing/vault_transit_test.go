package signing

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func vaultLookup(address string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := map[string]string{"VAULT_ADDR": address, "VAULT_TOKEN": "private-test-token", "CODEFLY_RELEASE_TRANSIT_KEY": "delivery", "CODEFLY_RELEASE_TRANSIT_MOUNT": "platform/transit"}[key]
		return value, ok
	}
}

func TestVaultTransitProtocol(t *testing.T) {
	for _, keyType := range []string{"ed25519", "ecdsa-p256"} {
		t.Run(keyType, func(t *testing.T) {
			public, private, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			exported := base64.StdEncoding.EncodeToString(public)
			if keyType == "ecdsa-p256" {
				der, _ := x509.MarshalPKIXPublicKey(&ec.PublicKey)
				exported = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("X-Vault-Token") != "private-test-token" {
					t.Error("missing token header")
				}
				if strings.Contains(r.URL.String(), "private-test-token") {
					t.Error("token in URL")
				}
				for key, values := range r.Header {
					if key != "X-Vault-Token" && strings.Contains(strings.Join(values, " "), "private-test-token") {
						t.Error("token in another header")
					}
				}
				raw, _ := io.ReadAll(r.Body)
				if strings.Contains(string(raw), "private-test-token") {
					t.Error("token in body")
				}
				switch r.URL.Path {
				case "/v1/platform/transit/keys/delivery":
					if r.Method != "GET" || len(raw) != 0 {
						t.Error("wrong key request")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"type": keyType, "latest_version": 3, "keys": map[string]any{"3": map[string]any{"public_key": exported}}}})
				case "/v1/platform/transit/sign/delivery":
					if r.Method != "POST" {
						t.Error("wrong sign method")
					}
					var body map[string]any
					if err := json.Unmarshal(raw, &body); err != nil {
						t.Error(err)
						return
					}
					if body["key_version"] != float64(3) {
						t.Error("version not pinned")
					}
					message, err := base64.StdEncoding.DecodeString(body["input"].(string))
					if err != nil {
						t.Error(err)
						return
					}
					signature := ed25519.Sign(private, message)
					if keyType == "ecdsa-p256" {
						if len(body) != 4 || body["hash_algorithm"] != "sha2-256" || body["marshaling_algorithm"] != "asn1" {
							t.Error("wrong ECDSA options")
						}
						digest := sha256.Sum256(message)
						signature, err = ecdsa.SignASN1(rand.Reader, ec, digest[:])
						if err != nil {
							t.Error(err)
						}
					} else if len(body) != 2 {
						t.Error("extra Ed25519 options")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"signature": "vault:v3:" + base64.StdEncoding.EncodeToString(signature)}})
				default:
					t.Error("unexpected request")
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			signer, err := NewVaultTransit(vaultLookup(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			if signer.KeyID() != "delivery-v3" {
				t.Fatal("wrong derived ID")
			}
			if _, err = SignEnvelope(testPayload(t), signer); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("%d requests", calls)
			}
		})
	}
}

func TestVaultTransitRefusalsNeverExposeToken(t *testing.T) {
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	for _, failure := range []string{"version", "get status", "sign status", "unsupported", "malformed", "redirect", "invalid key"} {
		t.Run(failure, func(t *testing.T) {
			targetCalls := 0
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls++ }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if failure == "redirect" {
					http.Redirect(w, r, target.URL, 302)
					return
				}
				if failure == "get status" || failure == "sign status" && r.Method == "POST" {
					w.WriteHeader(403)
					fmt.Fprint(w, "private-test-token")
					return
				}
				if failure == "malformed" {
					fmt.Fprint(w, "private-test-token")
					return
				}
				if r.Method == "POST" {
					fmt.Fprint(w, `{"data":{"signature":"vault:v4:c2ln"}}`)
					return
				}
				typ := "ed25519"
				if failure == "unsupported" {
					typ = "rsa-2048"
				}
				key := base64.StdEncoding.EncodeToString(public)
				if failure == "invalid key" {
					key = "private-test-token"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"type": typ, "latest_version": 3, "keys": map[string]any{"3": map[string]any{"public_key": key}}}})
			}))
			defer server.Close()
			signer, err := NewVaultTransit(vaultLookup(server.URL))
			if err == nil {
				_, err = signer.Sign([]byte("message"))
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

func TestVaultTransitRejectsUnsafeConfiguration(t *testing.T) {
	for _, address := range []string{"http://example.test", "http://192.0.2.1", "ftp://localhost", "https://private-test-token@example.test", "https://example.test?token=private-test-token"} {
		if _, err := NewVaultTransit(vaultLookup(address)); err == nil || strings.Contains(err.Error(), "private-test-token") {
			t.Fatalf("invalid address: %v", err)
		}
	}
}
