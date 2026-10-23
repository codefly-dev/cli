package azureauth

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/stretchr/testify/require"
)

const tenant = "11111111-1111-1111-1111-111111111111"

func TestCredentialUsesOnlyTheSelectedIdentity(t *testing.T) {
	values := map[string]string{
		"AZURE_TENANT_ID": tenant, "AZURE_CLIENT_ID": "22222222-2222-2222-2222-222222222222",
		"AZURE_FEDERATED_TOKEN_FILE": "/nonexistent/projected-token",
	}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	credential, err := Credential(tenant, lookup)
	require.NoError(t, err)
	require.IsType(t, &azidentity.AzureCLICredential{}, credential)
	values["CODEFLY_AZURE_CREDENTIAL"] = "workload"
	credential, err = Credential(tenant, lookup)
	require.NoError(t, err)
	require.IsType(t, &azidentity.WorkloadIdentityCredential{}, credential)
}

func TestWorkloadCredentialRefusesIncompleteOrCrossTenantIdentity(t *testing.T) {
	for _, change := range []map[string]string{
		{"AZURE_TENANT_ID": "33333333-3333-3333-3333-333333333333"},
		{"AZURE_TENANT_ID": ""}, {"AZURE_CLIENT_ID": ""}, {"AZURE_CLIENT_ID": "private-marker"},
		{"AZURE_FEDERATED_TOKEN_FILE": ""}, {"AZURE_FEDERATED_TOKEN_FILE": "relative-token"},
		{"CODEFLY_AZURE_CREDENTIAL": "private-marker"},
	} {
		values := map[string]string{
			"CODEFLY_AZURE_CREDENTIAL": "workload", "AZURE_TENANT_ID": tenant,
			"AZURE_CLIENT_ID":            "22222222-2222-2222-2222-222222222222",
			"AZURE_FEDERATED_TOKEN_FILE": "/nonexistent/projected-token",
		}
		for key, value := range change {
			values[key] = value
		}
		credential, err := Credential(tenant, func(key string) (string, bool) { value, ok := values[key]; return value, ok })
		require.Error(t, err)
		require.Nil(t, credential)
		require.NotContains(t, err.Error(), "private-marker")
	}
}
