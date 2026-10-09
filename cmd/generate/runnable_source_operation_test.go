package generate

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestGenerateRealSourceOperations(t *testing.T) {
	compressed, err := os.ReadFile("testdata/source-operation/accounts.binpb.gz")
	require.NoError(t, err)
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	require.NoError(t, err)
	contract, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, "sha256:925e72a5ba36b8e53362643e501dc7337b35c4a77277fc172a0ba72bf8c4ba8f", composition.APIContractDigest(contract))

	// The real owner's module, service and endpoint coordinates, with no live
	// dependencies: preparation consumes only its published contract and the
	// composition's declarations, never a running owner or a released agent.
	surfaceBytes, err := os.ReadFile("testdata/source-operation/surfaces.json")
	require.NoError(t, err)
	// The fixture projects the host's actual routing owners into the generic
	// export input. See README: this projection is not yet emitted by #1052.
	service := &resources.Service{Name: "accounts", Version: "0.0.0",
		Agent:     &resources.Agent{Kind: resources.ServiceAgent, Name: "go-grpc", Version: "0.1.53", Publisher: "codefly.dev"},
		Endpoints: []*resources.Endpoint{{Name: "authority", API: "grpc", Visibility: resources.VisibilityInternal}, {Name: "connect", API: "connect", Visibility: resources.VisibilityInternal}},
		Spec:      map[string]any{apiContractSurfacesSetting: "generated/surfaces.json"},
	}
	root, moduleDir := saveFixtureWorkspace(t, context.Background(), "saas-starter",
		&resources.ModuleInterface{Endpoints: []*resources.InterfaceEndpoint{{Service: "accounts", Endpoint: "authority", Visibility: resources.VisibilityInternal}, {Service: "accounts", Endpoint: "connect", Visibility: resources.VisibilityInternal}}},
		service)
	writeFixtureFile(t, filepath.Join(service.Dir(), "generated/surfaces.json"), surfaceBytes)
	surfaces, err := loadContractSurfaces(service, []*basev0.Endpoint{{Name: "authority", Api: "grpc"}, {Name: "connect", Api: "connect"}})
	require.NoError(t, err)
	const contractPath = "contracts/api/accounts/authority/contract.binpb"
	writeFixtureFile(t, filepath.Join(moduleDir, contractPath), contract)
	catalog := &composition.APIContractCatalog{Schema: composition.APIContractCatalogSchema, Package: "codefly/saas-starter", Version: "0.0.0", Endpoints: []composition.APIContractEndpoint{{
		Service: "accounts", Endpoint: "authority", API: "grpc", Kind: composition.APIContractKindProtobuf, Package: "saas.accounts.v1", Path: contractPath, Digest: composition.APIContractDigest(contract),
		Services: surfaces["authority"],
	}}}
	// Identical descriptor bytes do not mean identical served surfaces.
	// Authority serves neither Invoke nor Prune; Connect serves both pairs.
	connect := catalog.Endpoints[0]
	connect.Endpoint, connect.API = "connect", "connect"
	connect.Services = surfaces["connect"]
	connect.Path = "contracts/api/accounts/connect/contract.binpb"
	writeFixtureFile(t, filepath.Join(moduleDir, connect.Path), contract)
	catalog.Endpoints = append(catalog.Endpoints, connect)
	data, err := catalog.CanonicalBytes()
	require.NoError(t, err)
	writeFixtureFile(t, filepath.Join(moduleDir, composition.APIContractCatalogFileName), data)
	published := map[string]*basev0.RunnablePackage{}
	for _, method := range []string{"InvokeSourceOperation", "PruneSourceOperationReceipts"} {
		data, err := os.ReadFile("testdata/source-operation/" + method + ".json")
		require.NoError(t, err)
		pkg := new(basev0.RunnablePackage)
		require.NoError(t, protojson.Unmarshal(data, pkg))
		require.NoError(t, corerunnable.VerifyPackage(pkg))
		published["/saas.accounts.v1.DatasourceService/"+method] = pkg
	}
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"saas-starter"}))
	derived, err := runnablespkg.LoadDerivedOperations(moduleDir)
	require.NoError(t, err)
	require.Len(t, derived, 2)
	for _, operation := range derived {
		require.Equal(t, "connect", operation.Entry.Endpoint)
	}
	_, err = os.Stat(filepath.Join(moduleDir, "contracts/runnables/accounts/authority"))
	require.ErrorIs(t, err, os.ErrNotExist)
	selections := map[string]environments.ScopeSelections{}
	for _, operation := range derived {
		selections[RunnableBindingKey("saas-starter", operation.Entry.Name)] = environments.ScopeSelections{sourceSelection()}
	}
	writeSelectionEnvironment(t, root, selections)
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	values := readPreparedValues(t, filepath.Join(root, "configurations", "selected", RunnableBindingsGroup+".env"))
	require.Len(t, values, 2)
	for _, operation := range derived {
		require.Contains(t, published, operation.Entry.Method)
		require.Empty(t, operation.Operation.InvokeScopes)
		require.Empty(t, operation.Operation.LookupScopes)
		require.Len(t, operation.Operation.RequiredScopeSlots, 1)
		require.Equal(t, "source", operation.Operation.RequiredScopeSlots[0].Name)
		require.True(t, operation.Operation.RequiredScopeSlots[0].Lookup)
		key := RunnableBindingKey("saas-starter", operation.Entry.Name)
		value := values[key]
		prepared := verifyResolvedBinding(t, value, nil)
		require.True(t, proto.Equal(published[operation.Entry.Method].Contract, prepared.Contract), "prepared contract matches the producer's committed runnable package")
		require.Equal(t, "connect", prepared.Operation.Endpoint)
		require.Len(t, prepared.Policy.InvokeScopes, 1)
		require.True(t, proto.Equal(sourceSelection().Invoke[0], prepared.Policy.InvokeScopes[0]))
		require.True(t, proto.Equal(sourceSelection().Lookup[0], prepared.Policy.LookupScopes[0]))
		t.Logf("rendered %s=%s", key, value)
	}
	runnablesCheck, runnableBindingsCheck = true, true
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"saas-starter"}))
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
}
