package deploysecrets

import (
	"context"
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
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/stretchr/testify/require"
)

type testAzureCredential struct{}

type azureTestTransport func(*http.Request) (*http.Response, error)

func (f azureTestTransport) Do(r *http.Request) (*http.Response, error) { return f(r) }

func (testAzureCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "test-only", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func azureTestStore(t *testing.T, handler http.HandlerFunc) *AzureKeyVault {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Bearer authorization="https://login.microsoftonline.com/test", resource="https://vault.azure.net"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	require.NoError(t, err)
	transport := azureTestTransport(func(r *http.Request) (*http.Response, error) {
		local := r.Clone(r.Context())
		local.Host, local.URL.Host = r.URL.Host, endpoint.Host
		response, err := server.Client().Do(local)
		if response != nil {
			response.Request = r
		}
		return response, err
	})
	client, err := azsecrets.NewClient("https://example.vault.azure.net", testAzureCredential{}, &azsecrets.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1}},
	})
	require.NoError(t, err)
	return &AzureKeyVault{vaultURL: server.URL, client: client}
}

func TestAzureDescribeIsMetadataOnlyAndFollowsPages(t *testing.T) {
	var paths []string
	store := azureTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/secrets":
			fmt.Fprintf(w, `{"value":[],"nextLink":"https://%s/page2"}`, r.Host)
		case "/page2":
			fmt.Fprint(w, `{"value":[{"id":"https://example.vault.azure.net/secrets/key","attributes":{"enabled":true}}]}`)
		case "/secrets/KEY/versions":
			fmt.Fprint(w, `{"value":[{"id":"https://example.vault.azure.net/secrets/key/v1","attributes":{"enabled":true}}]}`)
		default:
			t.Errorf("unexpected payload operation %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	})
	description, err := store.Describe(context.Background(), "KEY")
	require.NoError(t, err)
	require.True(t, description.Exists)
	require.True(t, description.HasVersion)
	require.NotEmpty(t, description.Revision)
	require.Equal(t, []string{"/secrets", "/page2", "/secrets/KEY/versions"}, paths)
}

func TestAzureDescribeRefusesUnavailableMaterial(t *testing.T) {
	for _, tc := range []struct {
		name, active, deleted string
		status                int
		want                  string
	}{
		{"missing", `{"value":[]}`, `{"value":[]}`, 200, ""},
		{"disabled", `{"value":[{"id":"https://example.vault.azure.net/secrets/key","attributes":{"enabled":false}}]}`, "", 200, "disabled"},
		{"expired", `{"value":[{"id":"https://example.vault.azure.net/secrets/key","attributes":{"enabled":true,"exp":1}}]}`, "", 200, "expired"},
		{"future", `{"value":[{"id":"https://example.vault.azure.net/secrets/key","attributes":{"enabled":true,"nbf":4102444800}}]}`, "", 200, "not yet valid"},
		{"deleted", `{"value":[]}`, `{"value":[{"id":"https://example.vault.azure.net/secrets/key"}]}`, 200, "recoverable"},
		{"denied", `{"error":{"code":"Forbidden","message":"private-marker"}}`, "", 403, "HTTP 403"},
		{"invalid", `{"value":[{}]}`, "", 200, "incomplete"},
		{"missing-list", `{}`, "", 200, "incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := azureTestStore(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if r.URL.Path == "/deletedsecrets" {
					fmt.Fprint(w, tc.deleted)
				} else {
					fmt.Fprint(w, tc.active)
				}
			})
			description, err := store.Describe(context.Background(), "key")
			if tc.want == "" {
				require.NoError(t, err)
				require.False(t, description.Exists)
				return
			}
			require.ErrorContains(t, err, tc.want)
			require.NotContains(t, fmt.Sprintf("%v %+v %#v", err, err, err), "private-marker")
		})
	}
}

func TestAzurePlannerPreservesPropertiesAndRefusesChangedPlan(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			document := map[string]string{"unrelated": "keep-this"}
			version, writes := 1, 0
			store := azureTestStore(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/secrets":
					fmt.Fprint(w, `{"value":[{"id":"https://example.vault.azure.net/secrets/key","attributes":{"enabled":true}}]}`)
				case "/secrets/key/versions":
					fmt.Fprintf(w, `{"value":[{"id":"https://example.vault.azure.net/secrets/key/v%d","attributes":{"enabled":true}}]}`, version)
				case "/secrets/key", "/secrets/key/":
					if r.Method == http.MethodPut {
						var body struct {
							Value, ContentType string
							Tags               map[string]string
							Attributes         map[string]any
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if err := json.Unmarshal([]byte(body.Value), &document); err != nil {
							t.Error(err)
						}
						if body.Tags["owner"] != "existing-owner" || body.ContentType != "application/json" || body.Attributes["exp"] != float64(4102444800) {
							t.Error("write discarded existing secret metadata")
						}
						writes++
						version++
					}
					value, _ := json.Marshal(document)
					_ = json.NewEncoder(w).Encode(map[string]any{"value": string(value), "contentType": "application/json", "tags": map[string]string{"owner": "existing-owner"}, "attributes": map[string]any{"enabled": true, "exp": 4102444800}})
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(500)
				}
			})
			inputs := &Inputs{Rendered: gitops.RenderedEnvironment{Secrets: []gitops.RenderedServiceSecret{
				{RemoteKey: "key", Properties: filed(internalToken)},
			}}, Store: store, ReadPayloads: true, Generators: generators()}
			plan, err := Build(context.Background(), inputs)
			require.NoError(t, err)
			if changed {
				version++
			}
			written, err := plan.Apply(context.Background(), store, false)
			if changed {
				require.ErrorContains(t, err, "changed since planning")
				require.Empty(t, written)
				require.Zero(t, writes)
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{"key"}, written)
			require.Equal(t, "keep-this", document["unrelated"])
			require.NotEmpty(t, document[internalToken])
			again, err := Build(context.Background(), inputs)
			require.NoError(t, err)
			require.Empty(t, again.Changes())
			require.Equal(t, 1, writes)
		})
	}
}

func TestResolveAzureStoreUsesESOTenantSource(t *testing.T) {
	for _, tenant := range []string{"11111111-1111-1111-1111-111111111111", ""} {
		runner := &scriptedRunner{answers: map[string]func() ([]byte, error){
			"kubectl get clustersecretstore cell-secrets": func() ([]byte, error) {
				return []byte(`{"spec":{"provider":{"azurekv":{"vaultUrl":"https://example.vault.azure.net","authType":"WorkloadIdentity","serviceAccountRef":{"name":"eso","namespace":"external-secrets"}}}}}`), nil
			},
			"kubectl get serviceaccount eso": func() ([]byte, error) {
				return []byte(fmt.Sprintf(`{"metadata":{"annotations":{"azure.workload.identity/tenant-id":%q}}}`, tenant)), nil
			},
		}}
		store, err := ResolveStore(context.Background(), runner.run, ClusterTarget{Kubeconfig: "/kube", Context: "azure"},
			environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"}, "app")
		if tenant == "" {
			require.ErrorContains(t, err, "no tenant")
			continue
		}
		require.NoError(t, err)
		require.Equal(t, "azurekv https://example.vault.azure.net", store.Name())
		require.Contains(t, runner.calls[1].args, "external-secrets")
		require.Contains(t, runner.calls[1].args, "azure")
	}
}

func TestResolveAzureRefusesAmbiguousAuthority(t *testing.T) {
	for _, raw := range []string{
		`{"vaultUrl":"http://example.vault.azure.net","authType":"WorkloadIdentity","tenantId":"t"}`,
		`{"vaultUrl":"https://example.vault.azure.net.attacker.test","authType":"WorkloadIdentity","tenantId":"t"}`,
		`{"vaultUrl":"https://example.vault.azure.net","authType":"ServicePrincipal","tenantId":"t"}`,
		`{"vaultUrl":"https://example.vault.azure.net","authType":"WorkloadIdentity"}`,
		`{"vaultUrl":"https://example.vault.azure.net","authType":"WorkloadIdentity","tenantId":"t","serviceAccountRef":{"name":"eso"}}`,
	} {
		_, err := resolveAzureStore(context.Background(), nil, ClusterTarget{}, environments.EnvironmentSecretStoreReference{Kind: "ClusterSecretStore"}, "", json.RawMessage(raw))
		require.Error(t, err)
	}
}

func TestAzureFailureNeverExposesResponseOrPayload(t *testing.T) {
	store := azureTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		fmt.Fprint(w, `{"error":{"code":"Forbidden","message":"private-marker"}}`)
	})
	err := store.Write(context.Background(), "key", map[string]string{"key": "private-marker"}, true)
	require.Error(t, err)
	encoded, marshalErr := json.Marshal(err)
	require.NoError(t, marshalErr)
	for _, output := range []string{fmt.Sprintf("%v %+v %#v", err, err, err), string(encoded)} {
		require.False(t, strings.Contains(output, "private-marker"))
	}
}

func TestAzureForbiddenClassifiesOnlyKnownCodes(t *testing.T) {
	for code, want := range map[string]string{
		"ForbiddenByRbac":       "data-plane role denied",
		"ForbiddenByConnection": "private-network access denied",
		"ForbiddenByFirewall":   "private-network access denied",
		"private-marker":        "HTTP 403; backend output withheld",
	} {
		t.Run(code, func(t *testing.T) {
			store := azureTestStore(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(403)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "Forbidden", "message": "private-marker", "innererror": map[string]any{"code": code}}})
			})
			err := store.Write(context.Background(), "key", map[string]string{"key": "private-marker"}, true)
			require.ErrorContains(t, err, want)
			require.NotContains(t, fmt.Sprintf("%v %+v %#v", err, err, err), "private-marker")
		})
	}
}
