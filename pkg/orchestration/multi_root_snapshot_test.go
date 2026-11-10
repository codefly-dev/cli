package orchestration

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// A snapshot rooted in several services holds the union of their build
// closures, each service once, dependencies before what needs them — the
// same union a run takes (TestRunClosureUnionsEveryRootOverOneGraph) — and the
// closure of one root is what a snapshot of that root alone holds.
func TestSnapshotManagersAreTheUnionOfEveryRootsClosure(t *testing.T) {
	flow, ctx := multiRootFlow(t, "lastlogin-go/backend", "wiki/backend")
	flow.world.Mode = SnapshotMode

	required, err := flow.managerDependencies(ctx)
	require.NoError(t, err)
	got := uniques(required)

	require.NotContains(t, got, "lastlogin-go/backend", "the origin gets its manager separately")
	require.NotContains(t, got, "analytics/reports", "a service no root selects is not in the snapshot")
	require.Contains(t, got, "wiki/backend", "a co-root is managed, with its closure")
	require.Contains(t, got, "documents/documents")
	require.Contains(t, got, "saas/auth-gateway")
	seen := map[string]bool{}
	for _, unique := range got {
		require.False(t, seen[unique], "%s is managed twice", unique)
		seen[unique] = true
	}
	require.Less(t, indexOf(t, got, "documents/documents"), indexOf(t, got, "wiki/backend"))

	// Each root's closure is a part of it, and together they are all of it.
	union := map[string]bool{"lastlogin-go/backend": true}
	for _, root := range []string{"lastlogin-go/backend", "wiki/backend"} {
		closure, closureErr := flow.SnapshotClosure(ctx, root)
		require.NoError(t, closureErr)
		require.Contains(t, closure, root)
		for _, unique := range closure {
			require.True(t, unique == "lastlogin-go/backend" || seen[unique], "%s is in the closure of %s but not managed", unique, root)
			union[unique] = true
		}
	}
	require.Len(t, union, len(got)+1)
}

// With one root a snapshot holds exactly what it held before co-roots existed:
// the build closure of the origin, the origin left to be managed last.
func TestSnapshotManagersWithOneRootAreItsClosure(t *testing.T) {
	flow, ctx := multiRootFlow(t, "wiki/backend")
	flow.world.Mode = SnapshotMode

	required, err := flow.managerDependencies(ctx)
	require.NoError(t, err)
	closure, err := flow.SnapshotClosure(ctx, "wiki/backend")
	require.NoError(t, err)

	require.Equal(t, closure[:len(closure)-1], uniques(required), "the closure minus the origin, in the same order")
	require.Equal(t, "wiki/backend", closure[len(closure)-1])
}

// The closure of a root is the same whichever other roots the flow was given:
// what a module's render takes from a composition-wide deploy is what the
// render of that module alone took.
func TestSnapshotClosureDoesNotDependOnTheOtherRoots(t *testing.T) {
	alone, ctx := multiRootFlow(t, "wiki/backend")
	alone.world.Mode = SnapshotMode
	together, _ := multiRootFlow(t, "lastlogin-go/backend", "wiki/backend")
	together.world.Mode = SnapshotMode

	want, err := alone.SnapshotClosure(ctx, "wiki/backend")
	require.NoError(t, err)
	// After the scoping InitManagers applies to a run of several roots.
	required, err := together.managerDependencies(ctx)
	require.NoError(t, err)
	require.NoError(t, together.scopeDependenciesToRun(ctx, uniques(required), nil))
	got, err := together.SnapshotClosure(ctx, "wiki/backend")
	require.NoError(t, err)

	slices.Sort(want)
	slices.Sort(got)
	require.Equal(t, want, got)
}
