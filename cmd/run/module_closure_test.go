package run

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// `codefly run` derives the modules of the run from what it runs. The graph the
// flow hands every manager therefore holds the modules the root reaches and no
// others, however many the composition pins.
func TestRunFlowBuildsTheGraphFromTheRunClosure(t *testing.T) {
	ctx := t.Context()
	previousContext, previousCoRoots := runtimeContext, runCoRoots
	t.Cleanup(func() { runtimeContext, runCoRoots = previousContext, previousCoRoots })
	// An explicit context keeps newRunFlow from probing the Docker engine, which
	// only the "free" default needs.
	runtimeContext, runCoRoots = resources.RuntimeContextNative, nil

	workspace, err := resources.LoadWorkspaceFromDir(ctx, "../../pkg/orchestration/testdata/multi-root")
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "wiki")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "backend")
	require.NoError(t, err)

	flow, err := newRunFlow(ctx, workspace, module, service)
	require.NoError(t, err)

	_, err = flow.ServiceFromUnique("documents/documents")
	require.NoError(t, err, "a module the root reaches is in the run's graph")
	_, err = flow.ServiceFromUnique("analytics/reports")
	require.Error(t, err, "a pinned module no root reaches is not in the run's graph")
}
