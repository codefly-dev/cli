package generate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/stretchr/testify/require"
)

func TestRunnableAudienceUsesPublishedPrincipalWithoutRenamingModule(t *testing.T) {
	ctx := context.Background()
	contract := withHopPolicy(t, descriptorSet(t, ingestionFile(conformingOperation(), hopPolicyMethod("WriteText", "documents", "write"))))
	root, moduleDir := saveRunnableFixture(t, ctx, contract, "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	require.Equal(t, "documents", readIndex(t, moduleDir).Module)
	for _, method := range []string{"ApplyText", "WriteText"} {
		data, err := os.ReadFile(filepath.Join(moduleDir, runnablespkg.DerivedDir, "runtime-worker", "grpc", method, runnablespkg.OperationFileName))
		require.NoError(t, err)
		var operation map[string]any
		require.NoError(t, json.Unmarshal(data, &operation))
		require.Equal(t, "documents.ingestion", operation["audience"])
	}
}

func TestRunnableAudienceRefusalsLeaveOutputUnchanged(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*testing.T, string)
		want string
	}{
		{"missing", func(t *testing.T, dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, modulecontract.FileName)))
		}, "needs its owner's module contract"},
		{"malformed", func(t *testing.T, dir string) {
			writeFixtureFile(t, filepath.Join(dir, modulecontract.FileName), []byte("schema: codefly/module-contract/v1\nprincipal: documents\nunknown: true\n"))
		}, "needs its owner's module contract"},
		{"uncovered destination", func(t *testing.T, dir string) {
			saveRunnablePublication(t, dir, "documents.ingestion", modulecontract.Destination{ID: "other", Service: "runtime-worker", Endpoint: "other", Kind: "module"})
		}, "does not publish runnable destination runtime-worker/grpc"},
		{"explicit conflict", func(t *testing.T, dir string) {
			saveRunnablePublication(t, dir, "other", modulecontract.Destination{ID: "grpc", Service: "runtime-worker", Endpoint: "grpc", Kind: "module"})
		}, "differs from module contract principal"},
		{"escaped publication", func(t *testing.T, dir string) {
			outside := filepath.Join(t.TempDir(), "contract.yaml")
			data, err := os.ReadFile(filepath.Join(dir, modulecontract.FileName))
			require.NoError(t, err)
			writeFixtureFile(t, outside, data)
			require.NoError(t, os.Remove(filepath.Join(dir, modulecontract.FileName)))
			require.NoError(t, os.Symlink(outside, filepath.Join(dir, modulecontract.FileName)))
		}, "needs its owner's module contract"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")
			t.Chdir(root)
			resetRunnablesFlags(t)
			require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
			before := snapshotTree(t, moduleDir)
			test.edit(t, moduleDir)
			require.ErrorContains(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}), test.want)
			require.Equal(t, before, snapshotTree(t, moduleDir))
		})
	}
}

func TestRunnableAudienceChecksOpenAPIPublication(t *testing.T) {
	root, moduleDir := saveSolutionFixture(t, context.Background(), releasesDocument(t, nil))
	t.Chdir(root)
	resetRunnablesFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"releases"}))
	before := snapshotTree(t, moduleDir)
	saveRunnablePublication(t, moduleDir, "other", modulecontract.Destination{ID: "http", Service: "backend", Endpoint: "http", Kind: "module"})
	require.ErrorContains(t, RunnablesCmd.RunE(RunnablesCmd, []string{"releases"}), "differs from module contract principal")
	require.Equal(t, before, snapshotTree(t, moduleDir))
}
