package solutionrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// hostSurface is the surface the fixture's composition would declare: the host's
// entry and its gateway, each at one endpoint.
func hostSurface() HostSurface {
	return HostSurface{
		{Module: "host", Service: "frontend", Endpoint: "http"},
		{Module: "host", Service: "gateway", Endpoint: "rest"},
	}
}

// boundaryWorkspace loads a mutable copy of the fixture so a test can change a
// declaration and see what the rule then decides.
func boundaryWorkspace(t *testing.T, change func(dir string)) *resources.Workspace {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, os.DirFS(filepath.Join("testdata", "solution-boundary"))))
	if change != nil {
		change(dir)
	}
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), dir)
	require.NoError(t, err)
	return workspace
}

// refusedEdges is the refusal's edge lines alone, so an assertion about what was
// refused is not satisfied — or defeated — by the list of what is permitted.
func refusedEdges(err error) string {
	var edges []string
	for _, line := range strings.Split(err.Error(), "\n") {
		if strings.HasPrefix(line, "  ") {
			edges = append(edges, line)
		}
	}
	return strings.Join(edges, "\n")
}

func rewrite(t *testing.T, path string, replace func(string) string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(replace(string(data))), 0o600))
}

// The fixture's solution declares one dependency of each shape. Only what the
// surface permits is admitted, and every refusal names the edge.
func TestSolutionBoundariesAdmitOnlyTheDeclaredSurface(t *testing.T) {
	err := SolutionBoundaries(context.Background(), boundaryWorkspace(t, nil), hostSurface())
	require.Error(t, err)
	for _, want := range []string{
		"consumer/backend depends on bridge/relay/http",
		"consumer/backend depends on host/accounts/connect, whose api the solution already declares it consumes",
	} {
		require.Contains(t, err.Error(), want)
	}
	for _, admitted := range []string{"consumer/worker", "host/frontend", "host/gateway", "app/backend"} {
		require.NotContains(t, refusedEdges(err), admitted,
			"the surface permits it, or it is the solution's own module, or it is a build edge")
	}
	require.Contains(t, err.Error(), "host/frontend/http, host/gateway/rest", "the refusal states what IS permitted")
}

// A composition that names no surface has not said a guest may reach anything,
// so every cross-module edge is refused — including the ones a declared surface
// would permit. Absence refuses; it does not admit.
func TestSolutionBoundariesRefuseEverythingWithNoDeclaredSurface(t *testing.T) {
	err := SolutionBoundaries(context.Background(), boundaryWorkspace(t, nil), nil)
	require.Error(t, err)
	for _, want := range []string{
		"consumer/backend depends on host/frontend/http",
		"consumer/backend depends on host/gateway/rest",
		"consumer/backend depends on host/accounts/connect",
		"consumer/backend depends on bridge/relay/http",
	} {
		require.Contains(t, err.Error(), want)
	}
	require.Contains(t, err.Error(), "permits a solution to reach: none")
}

// The surface is per endpoint: permitting one endpoint of a service does not
// permit another.
func TestSolutionBoundariesAreDecidedPerEndpoint(t *testing.T) {
	workspace := boundaryWorkspace(t, func(dir string) {
		rewrite(t, filepath.Join(dir, "modules", "consumer", "services", "backend", "service.codefly.yaml"), func(text string) string {
			replaced := strings.ReplaceAll(text,
				"    - name: gateway\n      module: host\n      endpoints:\n          - name: rest\n",
				"    - name: gateway\n      module: host\n      endpoints:\n          - name: rest\n          - name: grpc\n")
			require.NotEqual(t, text, replaced, "the fixture's gateway dependency must be the one extended")
			return replaced
		})
	})
	err := SolutionBoundaries(context.Background(), workspace, hostSurface())
	require.Error(t, err)
	require.Contains(t, err.Error(), "depends on host/gateway/grpc")
	require.NotContains(t, err.Error(), "depends on host/gateway/rest")
}

// A module is a solution because it ships a manifest. An earlier shape also
// required a default runnable entry — which a module render does not need — so a
// solution without one was never checked.
func TestSolutionBoundariesDoNotDependOnServiceEntry(t *testing.T) {
	workspace := boundaryWorkspace(t, func(dir string) {
		rewrite(t, filepath.Join(dir, "modules", "consumer", "module.codefly.yaml"), func(text string) string {
			return strings.ReplaceAll(text, "service-entry: backend\n", "")
		})
	})
	err := SolutionBoundaries(context.Background(), workspace, hostSurface())
	require.Error(t, err)
	require.Contains(t, err.Error(), "consumer/backend depends on bridge/relay/http")
}

// Every service of the solution is checked, not just one: each renders its own
// workload, and each one's dependencies become its own addresses.
func TestSolutionBoundariesCheckEveryServiceOfTheSolution(t *testing.T) {
	workspace := boundaryWorkspace(t, func(dir string) {
		rewrite(t, filepath.Join(dir, "modules", "consumer", "services", "worker", "service.codefly.yaml"), func(text string) string {
			return text + "service-dependencies:\n    - name: relay\n      module: bridge\n      endpoints:\n          - name: http\n"
		})
	})
	err := SolutionBoundaries(context.Background(), workspace, hostSurface())
	require.Error(t, err)
	require.Contains(t, err.Error(), "consumer/worker depends on bridge/relay/http")
}

// What the solution declares it consumes explains a refusal; it never decides
// one. Changing or dropping the declaration leaves the edge refused, because the
// surface is what admits.
func TestSolutionBoundariesDoNotDependOnAConsumptionDeclaration(t *testing.T) {
	for name, change := range map[string]func(string) string{
		"consumption absent":        func(text string) string { return strings.ReplaceAll(text, "module: host", "module: app") },
		"consumption other service": func(text string) string { return strings.ReplaceAll(text, "service: accounts", "service: gateway") },
	} {
		t.Run(name, func(t *testing.T) {
			workspace := boundaryWorkspace(t, func(dir string) {
				rewrite(t, filepath.Join(dir, "modules", "consumer", "solution.codefly.yaml"), change)
			})
			err := SolutionBoundaries(context.Background(), workspace, hostSurface())
			require.Error(t, err)
			require.Contains(t, err.Error(), "depends on host/accounts/connect")
		})
	}
}

// Where a configuration group happens to be declared decides nothing. An earlier
// shape read the module holding the federation registrar as "the host" and
// admitted any of its services, so declaring that group in a second module
// widened the surface of both.
func TestSolutionBoundariesIgnoreWhereTheRegistrarIsDeclared(t *testing.T) {
	workspace := boundaryWorkspace(t, func(dir string) {
		rewrite(t, filepath.Join(dir, "modules", "bridge", "services", "relay", "service.codefly.yaml"), func(text string) string {
			return text + "\nworkspace-configuration-dependencies:\n    - federation\n"
		})
	})
	err := SolutionBoundaries(context.Background(), workspace, hostSurface())
	require.Error(t, err)
	require.Contains(t, err.Error(), "consumer/backend depends on bridge/relay/http",
		"holding the registrar does not make a module's services reachable from a guest")
}

// A declaration this rule is judged against that cannot be read refuses. An
// earlier shape skipped such a service, so one unreadable manifest turned the
// whole rule off for that solution.
func TestSolutionBoundariesRefuseAnUnreadableSolutionService(t *testing.T) {
	workspace := boundaryWorkspace(t, func(dir string) {
		rewrite(t, filepath.Join(dir, "modules", "consumer", "services", "backend", "service.codefly.yaml"), func(text string) string {
			return text + "service-dependencies:\n    - name: relay\n      module: bridge\n"
		})
	})
	err := SolutionBoundaries(context.Background(), workspace, hostSurface())
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot read the declarations of solution service consumer/backend")
}

// A module that ships no solution manifest is not a guest: its dependencies on
// other modules are the ordinary composition, judged by visibility, not by this.
func TestSolutionBoundariesIgnoreAPlainModule(t *testing.T) {
	err := SolutionBoundaries(context.Background(), boundaryWorkspace(t, nil), hostSurface())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "app/backend depends on",
		"app ships no manifest, so its cross-module dependency is not a boundary crossing")
}
