package signing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

const azureTestKey = "https://release.vault.azure.net/keys/delivery/0123456789abcdef0123456789abcdef"

type azureReleaseCredential struct{ resource string }

func (credential azureReleaseCredential) GetToken(_ context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	resource := credential.resource
	if resource == "" {
		resource = "https://vault.azure.net"
	}
	if len(options.Scopes) != 1 || options.Scopes[0] != resource+"/.default" {
		return azcore.AccessToken{}, fmt.Errorf("unexpected scope")
	}
	return azcore.AccessToken{Token: "private-release-test-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

type azureReleaseTransport func(*http.Request) (*http.Response, error)

func (f azureReleaseTransport) Do(r *http.Request) (*http.Response, error) { return f(r) }

func azureReleaseLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
}

func azureReleaseFixture(t *testing.T, mutate func(string, map[string]any), failSign bool) (*AzureKeyVault, error, *int) {
	t.Helper()
	return azureReleaseFixtureForKey(t, azureTestKey, "https://vault.azure.net", "EC", mutate, failSign)
}

func azureReleaseFixtureForKey(t *testing.T, keyURI, resource, keyType string, mutate func(string, map[string]any), failSign bool) (*AzureKeyVault, error, *int) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Bearer authorization="https://login.microsoftonline.com/tenant", resource="`+resource+`"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, "Bearer private-release-test-token", r.Header.Get("Authorization"))
		calls++
		w.Header().Set("Content-Type", "application/json")
		var response map[string]any
		switch r.Method {
		case http.MethodGet:
			require.Equal(t, "/keys/delivery/0123456789abcdef0123456789abcdef", r.URL.Path)
			response = map[string]any{
				"key": map[string]any{"kid": keyURI, "kty": keyType, "crv": "P-256", "key_ops": []string{"sign", "verify"},
					"x": base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
					"y": base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32)))},
				"attributes": map[string]any{"enabled": true},
			}
		case http.MethodPost:
			require.Equal(t, "/keys/delivery/0123456789abcdef0123456789abcdef/sign", r.URL.Path)
			if failSign {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"error":{"message":"private-release-test-token"}}`)
				return
			}
			var request struct {
				Algorithm string `json:"alg"`
				Value     string `json:"value"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Equal(t, "ES256", request.Algorithm)
			digest, err := base64.RawURLEncoding.DecodeString(request.Value)
			require.NoError(t, err)
			require.Len(t, digest, 32)
			r, s, err := ecdsa.Sign(rand.Reader, key, digest)
			require.NoError(t, err)
			raw := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
			response = map[string]any{"kid": keyURI, "value": base64.RawURLEncoding.EncodeToString(raw)}
		default:
			t.Errorf("unexpected operation %s", r.Method)
		}
		if mutate != nil {
			mutate(r.Method, response)
		}
		require.NoError(t, json.NewEncoder(w).Encode(response))
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport := azureReleaseTransport(func(r *http.Request) (*http.Response, error) {
		local := r.Clone(r.Context())
		local.Host, local.URL.Host = r.URL.Host, u.Host
		response, err := client.Do(local)
		if response != nil {
			response.Request = r
		}
		return response, err
	})
	signer, err := newAzureKeyVault(azureReleaseLookup(map[string]string{"CODEFLY_RELEASE_AZURE_KEY": keyURI}), azureReleaseCredential{resource: resource}, transport)
	return signer, err, &calls
}

func TestAzureManagedHSMReleaseSignsTheCoreDeliveryEnvelope(t *testing.T) {
	keyURI := strings.Replace(azureTestKey, ".vault.", ".managedhsm.", 1)
	signer, err, calls := azureReleaseFixtureForKey(t, keyURI, "https://managedhsm.azure.net", "EC-HSM", nil, false)
	require.NoError(t, err)
	check, err := KeyCheck(signer, solutionhost.KeyKindRelease, "acme", keyURI)
	require.NoError(t, err)
	payload := testPayload(t)
	envelope, err := SignEnvelope(payload, signer)
	require.NoError(t, err)
	require.NoError(t, check(context.Background(), envelope, payload))
	require.Equal(t, 2, *calls)
}

func TestAzureManagedHSMRefusesAnotherResourceChallenge(t *testing.T) {
	keyURI := strings.Replace(azureTestKey, ".vault.", ".managedhsm.", 1)
	_, err, calls := azureReleaseFixtureForKey(t, keyURI, "https://vault.azure.net", "EC-HSM", nil, false)
	require.Error(t, err)
	require.Zero(t, *calls)
}

func TestAzureReleaseSignsTheCoreDeliveryEnvelope(t *testing.T) {
	signer, err, calls := azureReleaseFixture(t, nil, false)
	require.NoError(t, err)
	check, err := KeyCheck(signer, solutionhost.KeyKindRelease, "acme", azureTestKey)
	require.NoError(t, err)
	payload := testPayload(t)
	envelope, err := SignEnvelope(payload, signer)
	require.NoError(t, err)
	require.NoError(t, check(context.Background(), envelope, payload))
	require.Equal(t, 2, *calls)
	_, err = KeyCheck(signer, solutionhost.KeyKindRelease, "acme", "another-version")
	require.Error(t, err)
	wrongDomain, err := KeyCheck(signer, solutionhost.KeyKindRelease, "other", azureTestKey)
	require.NoError(t, err)
	require.Error(t, wrongDomain(context.Background(), envelope, payload))
	// A caller cannot mutate the key the custody later verifies against.
	signer.Public().(*ecdsa.PublicKey).X.SetInt64(0)
	_, err = signer.Sign([]byte("next document"))
	require.NoError(t, err)
}

func TestAzureReleaseRefusesInvalidKeys(t *testing.T) {
	for _, name := range []string{"version", "curve", "disabled", "expired", "not-yet-valid", "operations", "point", "private"} {
		t.Run(name, func(t *testing.T) {
			_, err, _ := azureReleaseFixture(t, func(_ string, body map[string]any) {
				key, attrs := body["key"].(map[string]any), body["attributes"].(map[string]any)
				switch name {
				case "version":
					key["kid"] = azureTestKey + "other"
				case "curve":
					key["crv"] = "P-384"
				case "disabled":
					attrs["enabled"] = false
				case "expired":
					attrs["exp"] = time.Now().Add(-time.Hour).Unix()
				case "not-yet-valid":
					attrs["nbf"] = time.Now().Add(time.Hour).Unix()
				case "operations":
					key["key_ops"] = []string{"verify"}
				case "point":
					key["x"] = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
				case "private":
					key["d"] = "AQ"
				}
			}, false)
			require.Error(t, err)
		})
	}
}

func TestAzureReleaseRefusesWrongSignaturesAndRedactsFailures(t *testing.T) {
	for _, mode := range []string{"version", "invalid", "short", "denied"} {
		t.Run(mode, func(t *testing.T) {
			signer, err, calls := azureReleaseFixture(t, func(method string, body map[string]any) {
				if method != http.MethodPost {
					return
				}
				switch mode {
				case "version":
					body["kid"] = azureTestKey + "other"
				case "invalid":
					body["value"] = base64.RawURLEncoding.EncodeToString(make([]byte, 64))
				case "short":
					body["value"] = "AQ"
				}
			}, mode == "denied")
			require.NoError(t, err)
			_, err = signer.Sign([]byte("document"))
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private-release-test-token")
			require.Equal(t, 2, *calls, "failed signing must not retry")
		})
	}
}

func TestAzureReleaseRefusesUnsafeConfigurationBeforeNetwork(t *testing.T) {
	for _, key := range []string{
		strings.Replace(azureTestKey, "https:", "http:", 1), strings.Replace(azureTestKey, "vault.azure.net", "evil.example", 1),
		azureTestKey + "?query=1", azureTestKey + "?", azureTestKey + "#fragment", azureTestKey + "/extra",
		"https://release.vault.azure.net/keys/delivery", strings.Replace(azureTestKey, "/keys/", "/%6beys/", 1),
		strings.Replace(azureTestKey, "vault.azure.net", "managedhsm.azure.net.evil.example", 1),
	} {
		_, err := NewAzureKeyVault(azureReleaseLookup(map[string]string{"CODEFLY_RELEASE_AZURE_KEY": key}))
		require.Error(t, err)
	}
	_, err := NewAzureKeyVault(azureReleaseLookup(map[string]string{"CODEFLY_RELEASE_AZURE_KEY": azureTestKey}))
	require.ErrorContains(t, err, "TENANT")
	for _, other := range []string{"CODEFLY_RELEASE_KMS_KEY", "VAULT_TOKEN"} {
		_, err := newReleaseSigner(azureReleaseLookup(map[string]string{"CODEFLY_RELEASE_AZURE_KEY": azureTestKey, other: "set"}))
		require.ErrorContains(t, err, "two release custodies")
	}
}
