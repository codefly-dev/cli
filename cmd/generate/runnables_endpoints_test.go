package generate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// Descriptor availability is not listener membership. Both endpoints carry
// the same complete schema; only one serves the marked method and its lookup.
func TestGenerateRunnablesUsesEndpointServedSurface(t *testing.T) {
	contract := descriptorSet(t, ingestionFile(conformingOperation()))
	root, moduleDir := saveRunnableFixture(t, context.Background(), contract, "",
		&resources.Endpoint{Name: "receipts", API: "grpc"})
	catalog, err := composition.LoadAPIContractCatalog(moduleDir)
	require.NoError(t, err)
	receipts := catalog.Endpoints[0]
	receipts.Endpoint = "receipts"
	receipts.Services = []composition.APIContractService{{Name: "IngestionService", FullName: ingestPackage + ".IngestionService", Procedures: []string{conformingOperation().LookupMethod}}}
	catalog.Endpoints = append(catalog.Endpoints, receipts)
	save := func() {
		data, marshalErr := catalog.CanonicalBytes()
		require.NoError(t, marshalErr)
		writeFixtureFile(t, filepath.Join(moduleDir, composition.APIContractCatalogFileName), data)
	}
	save()
	t.Chdir(root)
	resetRunnablesFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	index := readIndex(t, moduleDir)
	require.Len(t, index.Operations, 1)
	require.Equal(t, "grpc", index.Operations[0].Endpoint)
	require.Equal(t, applyTextMethod, index.Operations[0].Method)
	require.Len(t, generatedTree(t, moduleDir), 3)

	// Moving the served surface, without changing any descriptor bytes, must
	// trip --check and remove the obsolete package on the next generation.
	catalog.Endpoints[0].Services, catalog.Endpoints[1].Services = catalog.Endpoints[1].Services, catalog.Endpoints[0].Services
	save()
	runnablesCheck = true
	require.Error(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	runnablesCheck = false
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	index = readIndex(t, moduleDir)
	require.Len(t, index.Operations, 1)
	require.Equal(t, "receipts", index.Operations[0].Endpoint)
	require.Len(t, generatedTree(t, moduleDir), 3)
}

func TestGenerateRunnablesRefusesInvalidEndpointSurface(t *testing.T) {
	for _, tc := range []struct {
		name       string
		procedures []string
		want       string
	}{
		{"missing lookup", []string{applyTextMethod}, "does not serve lookup method"},
		{"unknown method", []string{applyTextMethod, "/documents.ingest.v1.IngestionService/Missing"}, "absent from its descriptor"},
		{"duplicate method", []string{applyTextMethod, applyTextMethod}, "declares procedure twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, moduleDir := saveRunnableFixture(t, context.Background(), descriptorSet(t, ingestionFile(conformingOperation())), "")
			t.Chdir(root)
			resetRunnablesFlags(t)
			require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
			before := snapshotTree(t, moduleDir)
			catalog, err := composition.LoadAPIContractCatalog(moduleDir)
			require.NoError(t, err)
			catalog.Endpoints[0].Services[0].Procedures = tc.procedures
			data, err := catalog.CanonicalBytes()
			require.NoError(t, err)
			writeFixtureFile(t, filepath.Join(moduleDir, composition.APIContractCatalogFileName), data)
			err = RunnablesCmd.RunE(RunnablesCmd, []string{"documents"})
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, "runtime-worker/grpc")
			require.Equal(t, before, snapshotTree(t, moduleDir))
		})
	}
}
