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
// package manifest, beside the solution fixture.
func nativeSolution(t *testing.T, name string) (*resources.Module, *resources.Service) {
	t.Helper()
	module := solutionModule(t, name, "api")
	for file, contents := range map[string]string{
		"module.package.codefly.yaml": "kind: module-package\nschema: codefly/module-package/v2\nid: example/" + name + "\nversion: 1.0.0\nminimum-codefly-version: '>=0.0.0'\nartifact-roots: [services]\ncontracts:\n  composition: '>=1.0'\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), file), []byte(contents), 0o600))
	}
	return module, &resources.Service{Name: "api", ModuleIdentity: true, Endpoints: []*resources.Endpoint{{Name: "http", API: "http", Visibility: "internal"}}}
}

// The group a solution is shown under is the composition's: generated into
// the manifest from the workspace's solution-groups by the name the solution
// is composed under, for a headless solution as for one with a page, never
// for a module, and cleared by a withdrawal. Changing only the group changes
// the signed bytes, so it is a new generation.
func TestAComposedSolutionCarriesItsGroupInItsManifest(t *testing.T) {
	ctx := context.Background()
	env := identityEnvironment()
	expected := &solutionhost.SolutionGroup{ID: "operations", Title: "Operations", Order: 2}
	identity := []*resources.Service{{Name: "api", ModuleIdentity: true}}
	instance := func(workspace *resources.Workspace, module *resources.Module) *ModuleInstance {
		declared, _, err := moduleInstanceOf(ctx, workspace, module, identity, env, &InventoryPackage{ID: "example/" + module.Name, Version: "1.0.0"}, nil, nil)
		require.NoError(t, err)
		return declared
	}

	t.Run("render: grouped, ungrouped, headless and module", func(t *testing.T) {
		workspace := groupedWorkspace("Operations", "crm", "wiki")
		grouped := instance(workspace, solutionModule(t, "crm", "api"))
		require.Equal(t, expected, grouped.Group)

		ungrouped := instance(workspace, solutionModule(t, "crm-eu", "api"))
		require.Nil(t, ungrouped.Group, "a solution in no group carries no block")

		headless := solutionModule(t, "wiki", "api")
		require.NoError(t, os.Remove(filepath.Join(headless.Dir(), "services", "api", solutionrun.FrontendFileName)))
		wiki := instance(workspace, headless)
		require.Nil(t, wiki.Frontend)
		require.Equal(t, expected, wiki.Group, "the catalogue lists a headless solution, so it is grouped too")

		module := &ModuleInstance{Kind: solutionhost.KindModule, Name: "crm"}
		require.Nil(t, solutionGroupOf(workspace, module), "a module is grouped by nothing, whatever a workspace lists")
		require.Nil(t, solutionGroupOf(nil, grouped))
	})

	t.Run("native delivery carries the same block, and only the group makes new bytes", func(t *testing.T) {
		module, service := nativeSolution(t, "crm")
		workload := &NativeWorkload{
			Module:         module,
			Services:       []*resources.Service{service},
			FrontendBundle: builtBundle(),
			Started:        []NativeIdentity{{Module: "crm", Service: "api"}},
		}
		deliver := func(workspace *resources.Workspace) *solutionhost.ModuleManifest {
			manifest, err := (&NativeDeliverer{Workspace: workspace, Env: env}).Render(ctx, workload)
			require.NoError(t, err)
			require.NoError(t, manifest.Validate())
			return manifest
		}
		grouped := deliver(groupedWorkspace("Operations", "crm"))
		require.Equal(t, expected, grouped.Group)
		require.Equal(t, solutionhost.KindSolution, grouped.Kind)
		ungrouped := deliver(&resources.Workspace{Name: "example"})
		require.Nil(t, ungrouped.Group)
		renamed := deliver(groupedWorkspace("Ops", "crm"))
		require.Equal(t, "Ops", renamed.Group.Title)

		digests := map[string]string{}
		for name, document := range map[string]*solutionhost.ModuleManifest{"grouped": grouped, "ungrouped": ungrouped, "renamed": renamed} {
			digest, err := document.Digest()
			require.NoError(t, err)
			digests[name] = digest
		}
		require.NotEqual(t, digests["grouped"], digests["ungrouped"])
		require.NotEqual(t, digests["grouped"], digests["renamed"], "a title change alone is a new generation's bytes")

		withdrawal, err := WithdrawalOf(grouped)
		require.NoError(t, err)
		require.Nil(t, withdrawal.Group, "a withdrawal declares no group")
		require.NotNil(t, grouped.Group, "the prior generation keeps its own")
	})
}
