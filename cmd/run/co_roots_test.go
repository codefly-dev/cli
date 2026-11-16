package run

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// Naming several services runs them as roots of one graph. Each one after the
// first is resolved the same way the origin is, so a bare service name and a
// module-qualified one reach the same service.
func TestLoadCoRootsResolvesEveryServiceNamedAfterTheFirst(t *testing.T) {
	ctx := t.Context()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "../../pkg/orchestration/testdata/multi-root")
	require.NoError(t, err)

	uniques, roots, err := loadCoRoots(ctx, workspace,
		[]string{"beacon-go/backend", "quill/backend", "documents"}, "beacon-go/backend")
	require.NoError(t, err)

	require.Equal(t, []string{"quill/backend", "documents/documents"}, uniques)
	require.Len(t, roots, 2)
	require.Equal(t, "quill", roots[0].module.Name)
	require.Equal(t, "backend", roots[0].service.Name)
}

func TestLoadCoRootsWithOneServiceHasNoCoRoots(t *testing.T) {
	ctx := t.Context()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "../../pkg/orchestration/testdata/multi-root")
	require.NoError(t, err)

	uniques, roots, err := loadCoRoots(ctx, workspace, []string{"quill/backend"}, "quill/backend")
	require.NoError(t, err)
	require.Nil(t, uniques)
	require.Nil(t, roots)
}

// A service named twice — including once as the origin — is a mistake worth
// naming: it would otherwise silently collapse into a single root.
func TestLoadCoRootsRejectsAServiceNamedTwice(t *testing.T) {
	ctx := t.Context()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "../../pkg/orchestration/testdata/multi-root")
	require.NoError(t, err)

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "repeated co-root", args: []string{"beacon-go/backend", "quill/backend", "quill/backend"}},
		{name: "co-root repeats the origin", args: []string{"beacon-go/backend", "beacon-go/backend"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := loadCoRoots(ctx, workspace, test.args, "beacon-go/backend")
			require.ErrorContains(t, err, "named twice")
		})
	}
}

func TestLoadCoRootsReportsAnUnknownService(t *testing.T) {
	ctx := t.Context()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "../../pkg/orchestration/testdata/multi-root")
	require.NoError(t, err)

	_, _, err = loadCoRoots(ctx, workspace,
		[]string{"quill/backend", "nope/missing"}, "quill/backend")
	require.ErrorContains(t, err, "nope/missing")
}

// The run's module closure starts from the modules it was asked to run. A
// co-root named after the first one seeds it too, or running two solutions
// against one graph would derive the graph of only the first.
func TestRunModuleSeedsCoverEveryRoot(t *testing.T) {
	origin := &resources.Module{Name: "beacon-go"}

	require.Equal(t, []string{"beacon-go"}, runModuleSeeds(origin, nil))
	require.Equal(t, []string{"beacon-go", "quill", "documents"},
		runModuleSeeds(origin, []string{"quill/backend", "documents/documents"}))
}
