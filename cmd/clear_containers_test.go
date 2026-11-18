package cmd

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/processgroup"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
)

func labelled(name, namespace string) container.Summary {
	labels := map[string]string{dockerrun.LabelCodeflyOwner: "true"}
	if namespace != "" {
		labels[dockerrun.LabelCodeflyRecoveryNamespace] = namespace
	}
	return container.Summary{ID: name, Names: []string{"/" + name}, Labels: labels}
}

func containerNames(containers []container.Summary) []string {
	var names []string
	for _, c := range containers {
		names = append(names, c.Names[0][1:])
	}
	return names
}

// `codefly clear` in one workspace removed every /codefly* container on the
// machine, so clearing one checkout took the databases of every other
// checkout's run with it. A scoped clear removes the containers labelled with
// its own workspace's recovery namespace and leaves the rest: another
// workspace's, and one it cannot attribute. --all (no namespace) still takes
// every codefly container; a container that is not codefly's is never touched.
func TestAScopedClearRemovesOnlyItsOwnWorkspacesContainers(t *testing.T) {
	containers := []container.Summary{
		labelled("codefly-mine-store", "namespace-mine"),
		labelled("codefly-theirs-store", "namespace-theirs"),
		labelled("codefly-unattributed", ""),
		labelled("someone-elses-postgres", "namespace-mine"),
	}

	selected, left := clearableContainers(containers, nil, "namespace-mine")
	require.Equal(t, []string{"codefly-mine-store"}, containerNames(selected))
	require.Equal(t, []string{"codefly-theirs-store", "codefly-unattributed"}, containerNames(left))

	selected, left = clearableContainers(containers, nil, "")
	require.Equal(t, []string{"codefly-mine-store", "codefly-theirs-store", "codefly-unattributed"}, containerNames(selected))
	require.Empty(t, left)

	// The name filter narrows within the scope, and says nothing of the rest.
	selected, left = clearableContainers(containers, []string{"store"}, "namespace-mine")
	require.Equal(t, []string{"codefly-mine-store"}, containerNames(selected))
	require.Equal(t, []string{"codefly-theirs-store"}, containerNames(left))
}

// The namespace a clear scopes by is the one a run of that workspace labels
// its containers with: stable for one workspace, different for another, and
// absent for the machine-wide scope.
func TestTheClearNamespaceIsTheWorkspacesOwn(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	first, second := t.TempDir(), t.TempDir()
	mine, err := clearContainerNamespace(clearOptions{scope: processgroup.InWorkspace(first)})
	require.NoError(t, err)
	require.NotEmpty(t, mine)
	again, err := clearContainerNamespace(clearOptions{scope: processgroup.InWorkspace(first)})
	require.NoError(t, err)
	require.Equal(t, mine, again)
	theirs, err := clearContainerNamespace(clearOptions{scope: processgroup.InWorkspace(second)})
	require.NoError(t, err)
	require.NotEqual(t, mine, theirs)
	all, err := clearContainerNamespace(clearOptions{})
	require.NoError(t, err)
	require.Empty(t, all)
}
