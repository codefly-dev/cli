package generate

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	}
	root, moduleDir := saveFixtureWorkspace(t, context.Background(), "saas-starter",
		&resources.ModuleInterface{Endpoints: []*resources.InterfaceEndpoint{{Service: "accounts", Endpoint: "authority", Visibility: resources.VisibilityInternal}, {Service: "accounts", Endpoint: "connect", Visibility: resources.VisibilityInternal}}},
		service)
	writeFixtureFile(t, filepath.Join(service.Dir(), "generated/surfaces.json"), surfaceBytes)
	surfacePath := fixtureSurfacePath(t, moduleDir, service.Name, "generated/surfaces.json")
	surfaces, err := loadContractSurfaces(service, []*basev0.Endpoint{{Name: "authority", Api: "grpc"}, {Name: "connect", Api: "connect"}}, surfacePath)
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
		// The producer's committed package predates the wire spelling (core
		// #843: JSON names, 64-bit integers as strings); module-saas-starter
		// regenerates to it. Until then the two agree field for field once the
		// committed one is re-spelled, which is what this holds.
		require.True(t, proto.Equal(operation.Package.GetContract(), prepared.Contract), "prepared contract is the derived one")
		requireSameModuloWireSpelling(t, published[operation.Entry.Method].GetContract().GetInput().GetFields(), prepared.Contract.GetInput().GetFields())
		requireSameModuloWireSpelling(t, published[operation.Entry.Method].GetContract().GetOutput().GetFields(), prepared.Contract.GetOutput().GetFields())
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

// requireSameModuloWireSpelling holds a contract committed before the wire
// spelling to the one derived now: the same members in the same order, each
// named by its JSON name, a 64-bit integer as a STRING of format int64 or
// uint64 and nothing else, every non-required key optional, array items held
// the same way. A stopgap: it goes when module-saas-starter regenerates.
func requireSameModuloWireSpelling(t *testing.T, committed, derived []*basev0.RunnableField) {
	t.Helper()
	require.Len(t, derived, len(committed))
	for i, before := range committed {
		requireFieldModuloWireSpelling(t, before, derived[i], true)
	}
}

func requireFieldModuloWireSpelling(t *testing.T, before, after *basev0.RunnableField, member bool) {
	t.Helper()
	if member {
		parts := strings.Split(before.GetName(), "_")
		for j := 1; j < len(parts); j++ {
			if parts[j] != "" {
				parts[j] = strings.ToUpper(parts[j][:1]) + parts[j][1:]
			}
		}
		require.Equal(t, strings.Join(parts, ""), after.GetName())
		require.True(t, after.GetOptional(), "%s: proto3 JSON may omit every non-required key", after.GetName())
	}
	if before.GetType() == after.GetType() {
		require.Equal(t, before.GetFormat(), after.GetFormat(), "%s", after.GetName())
	} else {
		require.Equal(t, basev0.RunnableField_INTEGER, before.GetType(), "%s", before.GetName())
		require.Equal(t, basev0.RunnableField_STRING, after.GetType(), "%s", before.GetName())
		require.Contains(t, []string{corerunnable.FormatInt64, corerunnable.FormatUint64}, after.GetFormat(), "%s", before.GetName())
	}
	requireSameModuloWireSpelling(t, before.GetFields(), after.GetFields())
	require.Equal(t, before.GetItems() == nil, after.GetItems() == nil, "%s items", after.GetName())
	if before.GetItems() != nil {
		requireFieldModuloWireSpelling(t, before.GetItems(), after.GetItems(), false)
	}
}
