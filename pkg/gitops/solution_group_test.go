package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// groupedWorkspace composes the named solutions under one presentation group.
func groupedWorkspace(title string, solutions ...string) *resources.Workspace {
	return &resources.Workspace{Name: "example", SolutionGroups: []*resources.SolutionGroup{
		{ID: "operations", Title: title, Order: 2, Solutions: solutions},
	}}
}

// nativeSolution lays down what a native delivery of a solution reads: the
// started executable and the package manifest, beside the solution fixture.
func nativeSolution(t *testing.T, name string) (*resources.Module, *resources.Service) {
	t.Helper()
	module := solutionModule(t, name, "api")
	for file, contents := range map[string]string{
		"services/api/binary":         "native executable fixture",
		"module.package.codefly.yaml": "kind: module-package\nschema: codefly/module-package/v2\nid: example/" + name + "\nversion: 1.0.0\nminimum-codefly-version: '>=0.0.0'\nartifact-roots: [services]\ncontracts:\n  composition: '>=1.0'\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), file), []byte(contents), 0o600))
	}
	return module, &resources.Service{Name: "api", ModuleIdentity: true, Endpoints: []*resources.Endpoint{{Name: "http", API: "http", Visibility: "internal"}}}
}

// The group a solution is shown under is the composition's: generated into
// the presence from the workspace's solution-groups by the name the solution
// is composed under, for a headless solution as for one with a page, never
// for a module, and cleared by a withdrawal. Changing only the group changes
// the signed bytes, so it is a new generation.
func TestAComposedSolutionCarriesItsGroupInThePresence(t *testing.T) {
	ctx := context.Background()
	env := identityEnvironment()
	expected := &solutionhost.SolutionGroup{ID: "operations", Title: "Operations", Order: 2}
	options := func(name string) *RenderOptions {
		return &RenderOptions{Module: name, Package: &InventoryPackage{ID: "example/" + name, Version: "1.0.0"}}
	}

	t.Run("render: grouped, ungrouped, headless and module", func(t *testing.T) {
		workspace := groupedWorkspace("Operations", "crm", "wiki")
		grouped, _, err := presenceInstanceOf(solutionModule(t, "crm", "api"), nil, env, options("crm"))
		require.NoError(t, err)
		require.Equal(t, expected, solutionGroupOf(workspace, grouped))

		ungrouped, _, err := presenceInstanceOf(solutionModule(t, "crm-eu", "api"), nil, env, options("crm-eu"))
		require.NoError(t, err)
		require.Nil(t, solutionGroupOf(workspace, ungrouped), "a solution in no group carries no block")

		headless := solutionModule(t, "wiki", "api")
		require.NoError(t, os.Remove(filepath.Join(headless.Dir(), "services", "api", solutionrun.FrontendFileName)))
		instance, _, err := presenceInstanceOf(headless, nil, env, options("wiki"))
		require.NoError(t, err)
		require.Nil(t, instance.Frontend)
		require.Equal(t, expected, solutionGroupOf(workspace, instance), "the catalogue lists a headless solution, so it is grouped too")

		module := &SolutionInstance{Kind: solutionhost.KindModule, Name: "crm"}
		require.Nil(t, solutionGroupOf(workspace, module), "a module is grouped by nothing, whatever a workspace lists")
		require.Nil(t, solutionGroupOf(nil, grouped))
	})

	t.Run("native delivery carries the same block, and only the group makes new bytes", func(t *testing.T) {
		module, service := nativeSolution(t, "crm")
		workload := &NativeWorkload{
			Module: module, Service: service, Services: []*resources.Service{service},
			Subject: "system:serviceaccount:example:crm-api", SPIFFEID: "spiffe://cluster.example/ns/example/sa/crm-api",
			Artifact: NativeArtifact{Executable: filepath.Join(module.Dir(), "services", "api", "binary")}, FrontendBundle: builtBundle(),
		}
		deliver := func(workspace *resources.Workspace) *solutionhost.SolutionHostBinding {
			documents, err := (&NativeDeliverer{Workspace: workspace, Env: env}).Render(ctx, workload)
			require.NoError(t, err)
			require.NoError(t, documents.Presence.Validate())
			return documents.Presence
		}
		grouped := deliver(groupedWorkspace("Operations", "crm"))
		require.Equal(t, expected, grouped.Group)
		require.Equal(t, solutionhost.KindSolution, grouped.Kind)
		ungrouped := deliver(&resources.Workspace{Name: "example"})
		require.Nil(t, ungrouped.Group)
		renamed := deliver(groupedWorkspace("Ops", "crm"))
		require.Equal(t, "Ops", renamed.Group.Title)

		digests := map[string]string{}
		for name, document := range map[string]*solutionhost.SolutionHostBinding{"grouped": grouped, "ungrouped": ungrouped, "renamed": renamed} {
			digest, err := document.Digest()
			require.NoError(t, err)
			digests[name] = digest
		}
		require.NotEqual(t, digests["grouped"], digests["ungrouped"])
		require.NotEqual(t, digests["grouped"], digests["renamed"], "a title change alone is a new generation's bytes")

		tombstone, err := tombstoneOf(grouped)
		require.NoError(t, err)
		require.Nil(t, tombstone.Group, "a withdrawal declares no group")
		require.NotNil(t, grouped.Group, "the prior generation keeps its own")
	})
}
