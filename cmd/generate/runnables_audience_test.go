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
		// A contract that exists and cannot be read is never absence: a module
		// that authored one and got it wrong is told so, whatever kind of
		// operation it serves.
		{"malformed", func(t *testing.T, dir string) {
			writeFixtureFile(t, filepath.Join(dir, modulecontract.FileName), []byte("schema: codefly/module-contract/v1\nprincipal: documents\nunknown: true\n"))
		}, "cannot read its owner's module contract"},
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
		}, "cannot read its owner's module contract"},
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

// TestRunnableExplicitAudienceNeedsNoModuleContract is the SaaS case, and the
// behaviour that replaces a universal gate.
//
// The author annotated the method's audience explicitly (core sets
// OperationSpec.Declared from the method option), so the audience is theirs —
// a host surface the module reaches without being installed as one. Core does
// not require it to equal any module principal, and demanding a module
// contract here demanded an identity the module does not have. Generation
// proceeds and carries the explicit audience, its scopes and its lookup
// through unchanged.
func TestRunnableExplicitAudienceNeedsNoModuleContract(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	require.NoError(t, os.Remove(filepath.Join(moduleDir, modulecontract.FileName)))

	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))

	data, err := os.ReadFile(filepath.Join(moduleDir, filepath.FromSlash(runnablespkg.DerivedDir),
		"runtime-worker", "grpc", "ApplyText", runnablespkg.OperationFileName))
	require.NoError(t, err)
	var operation map[string]any
	require.NoError(t, json.Unmarshal(data, &operation))
	require.Equal(t, "documents.ingestion", operation["audience"], "the explicit audience is preserved, not derived")
	require.Equal(t, "/documents.ingest.v1.IngestionService/LookupText", operation["lookup_method"], "and its lookup")
	require.NotEmpty(t, operation["invoke_scopes"], "and its scopes")
	require.NotEmpty(t, operation["lookup_scopes"])
}

// TestRunnableDefaultAudienceStillNeedsTheModuleContract keeps the waiver
// narrow. A method with no explicit audience takes core's default from
// ServiceOwner.Audience, which this generation fills from the module
// publication — so with no contract there is no principal to derive from, and
// it refuses by name rather than emitting an empty audience.
func TestRunnableDefaultAudienceStillNeedsTheModuleContract(t *testing.T) {
	ctx := context.Background()
	file := ingestionFile(conformingOperation(), hopPolicyMethod("CountText", "documents", "read"))
	root, moduleDir := saveRunnableFixture(t, ctx, withHopPolicy(t, descriptorSet(t, file)), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	before := snapshotTree(t, moduleDir)

	require.NoError(t, os.Remove(filepath.Join(moduleDir, modulecontract.FileName)))
	err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"})
	require.ErrorContains(t, err, "derives its audience by default and so needs its owner's module contract")
	require.Equal(t, before, snapshotTree(t, moduleDir), "a refusal leaves the output unchanged")
}

// TestRunnableDanglingModuleContractIsNotAbsence closes the gap between "no
// declaration" and "a declaration that cannot be read".
//
// Core's loader reads through os.Root, and a dangling symlink INSIDE the module
// reports fs.ErrNotExist — the same error as a module that published nothing.
// Treating that as absence would let a broken declaration take the explicit-
// audience path, which is the one path that accepts an audience with no
// principal to check it against. The entry exists, so generation refuses and
// writes nothing.
func TestRunnableDanglingModuleContractIsNotAbsence(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	before := snapshotTree(t, moduleDir)

	// A link to a sibling that does not exist: in-root, so os.Root resolves it
	// rather than refusing the escape, and finds nothing.
	contract := filepath.Join(moduleDir, modulecontract.FileName)
	require.NoError(t, os.Remove(contract))
	require.NoError(t, os.Symlink("absent.module.contract.codefly.yaml", contract))
	entry, err := os.Lstat(contract)
	require.NoError(t, err, "the declaration entry exists")
	require.NotZero(t, entry.Mode()&os.ModeSymlink)
	_, statErr := os.Stat(contract)
	require.True(t, os.IsNotExist(statErr), "and what it points at does not")

	err = RunnablesCmd.RunE(RunnablesCmd, []string{"documents"})
	require.ErrorContains(t, err, "cannot read its owner's module contract")
	require.Equal(t, before, snapshotTree(t, moduleDir), "a refusal leaves the output unchanged")
}

// TestRunnableAnnotatedMethodWithNoAudienceRefuses pins the floor under the
// explicit branch. Core's OperationFromMethod validates the audience before it
// sets Declared, so this should be unreachable through core — which is the
// point: the branch that accepts an audience without a principal must not be
// the one that accepts an empty one, whatever reaches it.
func TestRunnableAnnotatedMethodWithNoAudienceRefuses(t *testing.T) {
	ctx := context.Background()
	operation := conformingOperation()
	operation.Audience = ""
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(operation)), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	require.NoError(t, os.Remove(filepath.Join(moduleDir, modulecontract.FileName)))
	before := snapshotTree(t, moduleDir)

	require.Error(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}),
		"an annotated method with no audience cannot publish without a principal")
	require.Equal(t, before, snapshotTree(t, moduleDir), "and writes nothing")
}
