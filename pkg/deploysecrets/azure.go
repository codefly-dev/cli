package deploysecrets

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/internal/azureauth"
)

// AzureKeyVault uses the selected publisher identity, restricted to the ESO
// store's tenant. It never borrows ESO's credential or falls back to another
// credential source. The SDK owns authentication and paging.
type AzureKeyVault struct {
	vaultURL string
	client   *azsecrets.Client
}

var azureSecretName = regexp.MustCompile(`^[A-Za-z0-9-]{1,127}$`)
var azureVaultHost = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]{1,22}[a-zA-Z0-9]\.vault\.azure\.net$`)

// AzurePublisherStore reuses the live-store adapter inside an infrastructure-owned
// publisher Job. Infrastructure supplies the destination, never the input document.
func AzurePublisherStore(ctx context.Context, vaultURL, tenantID string) (Store, error) {
	raw, err := json.Marshal(map[string]string{"vaultUrl": vaultURL, "tenantId": tenantID, "authType": "WorkloadIdentity"})
	if err != nil {
		return nil, errors.New("invalid publisher store binding")
	}
	return resolveAzureStore(ctx, nil, ClusterTarget{}, environments.EnvironmentSecretStoreReference{}, "", raw)
}

func resolveAzureStore(ctx context.Context, run Runner, target ClusterTarget, ref environments.EnvironmentSecretStoreReference, namespace string, raw json.RawMessage) (Store, error) {
	var provider struct {
		VaultURL          string                           `json:"vaultUrl"`
		TenantID          string                           `json:"tenantId"`
		AuthType          string                           `json:"authType"`
		EnvironmentType   string                           `json:"environmentType"`
		ServiceAccountRef struct{ Name, Namespace string } `json:"serviceAccountRef"`
	}
	if json.Unmarshal(raw, &provider) != nil {
		return nil, errors.New("invalid azurekv provider configuration (backend output withheld)")
	}
	u, parseErr := url.Parse(provider.VaultURL)
	if parseErr != nil || u.Scheme != "https" || !azureVaultHost.MatchString(u.Host) || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("azurekv requires an HTTPS Azure public-cloud vault URL")
	}
	if provider.AuthType != "WorkloadIdentity" || (provider.EnvironmentType != "" && provider.EnvironmentType != "PublicCloud") {
		return nil, errors.New("azurekv publication currently supports PublicCloud WorkloadIdentity stores")
	}
	if provider.ServiceAccountRef.Name != "" {
		saNamespace := provider.ServiceAccountRef.Namespace
		if ref.Kind == "SecretStore" {
			if saNamespace != "" && saNamespace != namespace {
				return nil, errors.New("azurekv SecretStore service account must be in the store namespace")
			}
			saNamespace = namespace
		}
		if saNamespace == "" {
			return nil, errors.New("azurekv ClusterSecretStore service account requires a namespace")
		}
		output, err := run(ctx, nil, "kubectl", "get", "serviceaccount", provider.ServiceAccountRef.Name,
			"--namespace", saNamespace, "--kubeconfig", target.Kubeconfig, "--context", target.Context, "-o", "json")
		if err != nil {
			return nil, err
		}
		var account struct {
			Metadata struct{ Annotations map[string]string }
		}
		if json.Unmarshal(output, &account) != nil {
			return nil, errors.New("invalid azurekv service account metadata (backend output withheld)")
		}
		tenant := account.Metadata.Annotations["azure.workload.identity/tenant-id"]
		if provider.TenantID != "" && tenant != "" {
			return nil, errors.New("azurekv tenant is declared twice; use the same single tenant source as ESO")
		}
		if tenant != "" {
			provider.TenantID = tenant
		}
	}
	if provider.TenantID == "" {
		return nil, errors.New("azurekv declares no tenant identity")
	}
	credential, err := azureauth.Credential(provider.TenantID, nil)
	if err != nil {
		return nil, err
	}
	client, err := azsecrets.NewClient(strings.TrimRight(provider.VaultURL, "/"), credential, nil)
	if err != nil {
		return nil, errors.New("cannot initialize Azure Key Vault client")
	}
	return &AzureKeyVault{vaultURL: strings.TrimRight(provider.VaultURL, "/"), client: client}, nil
}

func (store *AzureKeyVault) Name() string { return "azurekv " + store.vaultURL }

func (store *AzureKeyVault) Describe(ctx context.Context, key string) (Description, error) {
	if !azureSecretName.MatchString(key) {
		return Description{}, errors.New("invalid Azure Key Vault secret name")
	}
	pager := store.client.NewListSecretPropertiesPager(nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return Description{}, azureError(err)
		}
		if page.Value == nil {
			return Description{}, errors.New("incomplete Azure secret listing")
		}
		for _, property := range page.Value {
			if property == nil || property.ID == nil {
				return Description{}, errors.New("incomplete Azure secret metadata")
			}
			if !strings.EqualFold(property.ID.Name(), key) {
				continue
			}
			attributes := property.Attributes
			if attributes == nil || attributes.Enabled == nil {
				return Description{}, errors.New("incomplete Azure secret attributes")
			}
			now := time.Now()
			if !*attributes.Enabled || (attributes.Expires != nil && !now.Before(*attributes.Expires)) ||
				(attributes.NotBefore != nil && now.Before(*attributes.NotBefore)) || (property.Managed != nil && *property.Managed) {
				return Description{}, errors.New("azure secret is disabled, expired, not yet valid or externally managed; refusing to replace it")
			}
			// Include every version's metadata: listing base properties alone can
			// miss two new versions created within the same timestamp second.
			metadata, _ := json.Marshal(property)
			versions := []string{string(metadata)}
			vp := store.client.NewListSecretPropertiesVersionsPager(key, nil)
			for vp.More() {
				page, err := vp.NextPage(ctx)
				if err != nil {
					return Description{}, azureError(err)
				}
				for _, version := range page.Value {
					if version == nil || version.ID == nil {
						return Description{}, errors.New("incomplete Azure version metadata")
					}
					encoded, _ := json.Marshal(version)
					versions = append(versions, string(encoded))
				}
			}
			if len(versions) == 1 {
				return Description{}, errors.New("azure secret has no version; refusing to replace it")
			}
			slices.Sort(versions)
			return Description{Exists: true, HasVersion: true, Version: fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(versions, "\n"))))}, nil
		}
	}
	return store.describeDeleted(ctx, key)
}

func (store *AzureKeyVault) describeDeleted(ctx context.Context, key string) (Description, error) {
	// Absence from the active list is insufficient: recoverable material must
	// never be purged, recovered or replaced as part of credential seeding.
	deleted := store.client.NewListDeletedSecretPropertiesPager(nil)
	for deleted.More() {
		page, err := deleted.NextPage(ctx)
		if err != nil {
			return Description{}, azureError(err)
		}
		if page.Value == nil {
			return Description{}, errors.New("incomplete deleted Azure secret listing")
		}
		for _, property := range page.Value {
			if property == nil || property.ID == nil {
				return Description{}, errors.New("incomplete deleted Azure secret metadata")
			}
			if strings.EqualFold(property.ID.Name(), key) {
				return Description{}, errors.New("azure secret is deleted and recoverable; refusing to replace it")
			}
		}
	}
	return Description{}, nil
}

func (store *AzureKeyVault) Read(ctx context.Context, key string) (map[string]string, error) {
	if !azureSecretName.MatchString(key) {
		return nil, errors.New("invalid Azure Key Vault secret name")
	}
	result, err := store.client.GetSecret(ctx, key, "", nil)
	if err != nil {
		return nil, azureError(err)
	}
	if result.Value == nil {
		return nil, errors.New("azure secret has no value")
	}
	return decodeDocument(key, []byte(*result.Value))
}

func (store *AzureKeyVault) Write(ctx context.Context, key string, document map[string]string, create bool) (string, error) {
	if !azureSecretName.MatchString(key) {
		return "", errors.New("invalid Azure Key Vault secret name")
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", errors.New("cannot encode Azure secret document")
	}
	value := string(encoded)
	parameters := azsecrets.SetSecretParameters{Value: &value}
	if !create {
		current, readErr := store.client.GetSecret(ctx, key, "", nil)
		if readErr != nil {
			return "", azureError(readErr)
		}
		parameters.ContentType, parameters.Tags = current.ContentType, current.Tags
		if attributes := current.Attributes; attributes != nil {
			parameters.SecretAttributes = &azsecrets.SecretAttributes{
				Enabled: attributes.Enabled, Expires: attributes.Expires, NotBefore: attributes.NotBefore,
			}
		}
	}
	_, err = store.client.SetSecret(ctx, key, parameters, nil)
	if err != nil {
		return "", azureError(err)
	}
	description, err := store.Describe(ctx, key)
	return description.Version, err
}

// Never wrap SDK errors: their response bodies and auth diagnostics may contain
// credentials. This also protects %+v, %#v and JSON error formatting.
func azureError(err error) error {
	if err == nil {
		return nil
	}
	var response *azcore.ResponseError
	if errors.As(err, &response) {
		reason := ""
		if response.StatusCode == 403 && response.RawResponse != nil {
			// Inspect only the SDK's response payload for a closed diagnostic code.
			// No provider text, IDs, URLs or unknown code can enter the error.
			payload, readErr := runtime.Payload(response.RawResponse)
			var detail struct {
				Error struct {
					InnerError struct {
						Code string `json:"code"`
					} `json:"innererror"`
				} `json:"error"`
			}
			if readErr == nil && len(payload) <= 65536 && json.Unmarshal(payload, &detail) == nil {
				switch detail.Error.InnerError.Code {
				case "ForbiddenByRbac":
					reason = "; data-plane role denied"
				case "ForbiddenByConnection", "ForbiddenByFirewall":
					reason = "; private-network access denied"
				}
			}
		}
		return fmt.Errorf("azure Key Vault request failed (HTTP %d%s; backend output withheld)", response.StatusCode, reason)
	}
	return errors.New("azure Key Vault authentication or transport failed (backend output withheld)")
}
