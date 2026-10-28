// Package azureauth selects one explicit Azure identity for publication.
package azureauth

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/google/uuid"
)

// Credential never falls back between an operator login and workload identity.
// The caller supplies the tenant declared by the destination, not an ambient default.
func Credential(tenant string, lookup func(string) (string, bool)) (azcore.TokenCredential, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	id, err := uuid.Parse(tenant)
	if err != nil || id == uuid.Nil || id.String() != tenant {
		return nil, errors.New("Azure publication requires a declared tenant UUID")
	}
	mode, _ := lookup("CODEFLY_AZURE_CREDENTIAL")
	switch mode {
	case "", "azure-cli":
		credential, err := azidentity.NewAzureCLICredential(&azidentity.AzureCLICredentialOptions{TenantID: tenant})
		if err != nil {
			return nil, errors.New("cannot initialize the Azure CLI publication credential")
		}
		return credential, nil
	case "workload":
		workloadTenant, _ := lookup("AZURE_TENANT_ID")
		client, _ := lookup("AZURE_CLIENT_ID")
		tokenFile, _ := lookup("AZURE_FEDERATED_TOKEN_FILE")
		clientID, err := uuid.Parse(client)
		if workloadTenant != tenant || err != nil || clientID == uuid.Nil || clientID.String() != client || !filepath.IsAbs(tokenFile) {
			return nil, errors.New("Azure workload publication requires the declared tenant, a client UUID and an absolute federated token file")
		}
		credential, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			TenantID: tenant, ClientID: client, TokenFilePath: tokenFile,
		})
		if err != nil {
			return nil, errors.New("cannot initialize the Azure workload publication credential")
		}
		return credential, nil
	default:
		return nil, errors.New("CODEFLY_AZURE_CREDENTIAL must be azure-cli or workload")
	}
}
