package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fixturecomposition "github.com/codefly-dev/authority/libraries/fixture-composition/go"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/fixturefragments"
)

const fragmentHead = "schema: codefly.dev/authority/fixture-composition/v1\n"

// fragmentWorkspace composes two modules from OUTSIDE the workspace tree, as a
// pinned module materializes, each shipping a fixture-composition fragment —
// what every module composed since #1197 does — with authority/local declaring
// the group. An in-repo module's configurations belong to the consuming
// workspace root, so only a foreign module offers a group at all.
func fragmentWorkspace(t *testing.T, own string) *resources.Workspace {
	t.Helper()
	authority, documents := filepath.Join(t.TempDir(), "authority"), filepath.Join(t.TempDir(), "documents")
	service := "name: %s\nversion: 0.0.0\nagent: {kind: runtime::service, name: go-grpc, version: 0.0.16, publisher: codefly.ai}\n"
	write := func(dir, rel, content string) {
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	write(authority, "module.codefly.yaml", "name: authority\nservices:\n - name: local\n")
	write(authority, "services/local/service.codefly.yaml",
		strings.Replace(service, "%s", "local", 1)+"workspace-configuration-dependencies: ["+fixturefragments.Group+"]\n")
	write(authority, "configurations/local/"+fixturefragments.Group+".yaml", fragmentHead)
	write(documents, "module.codefly.yaml", "name: documents\nservices:\n - name: documents\n")
	write(documents, "services/documents/service.codefly.yaml", strings.Replace(service, "%s", "documents", 1))
	write(documents, "configurations/local/"+fixturefragments.Group+".yaml", fragmentHead+`roles:
  - organization: acme
    name: viewer
    scopes:
      - {kind: documents, actions: [read]}
`)
	files := map[string]string{
		"workspace.codefly.yaml": "name: fragments\nlayout: modules\nmodules:\n - name: authority\n   path: " + authority +
			"\n - name: documents\n   path: " + documents + "\n",
	}
	if own != "" {
		files["configurations/local/"+fixturefragments.Group+".yaml"] = own
	}
	return writeTempWorkspace(t, files)
}

func readerFor(t *testing.T, workspace *resources.Workspace) *WorkspaceConfigurationReader {
	t.Helper()
	reader, err := NewWorkspaceConfigurationReader(context.Background(), workspace)
	require.NoError(t, err)
	require.NoError(t, reader.Load(context.Background(), resources.LocalEnvironment()))
	return reader
}

func fixtureGroups(reader *WorkspaceConfigurationReader) []string {
	var out []string
	for _, info := range workspaceConfigurationInfos(reader.Configurations()) {
		if info.GetName() == fixturefragments.Group {
			out = append(out, string(info.GetData().GetContent()))
		}
	}
	return out
}

// fixture-composition is the one multi-provider workspace configuration: every
// composed module ships a fragment and the run merges them. Core reports the
// second provider as an ambiguity, which is right for every other group and
// wrong for this one, so the shared reader supplies the merge — once, and no
// longer ambiguous for anything reading through it.
func TestTheReaderSuppliesTheMergeOfEveryModulesFragment(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	reader := readerFor(t, fragmentWorkspace(t, ""))

	groups := fixtureGroups(reader)
	require.Len(t, groups, 1, "the merge is supplied exactly once")
	doc, err := fixturecomposition.Decode([]byte(groups[0]))
	require.NoError(t, err)
	require.Len(t, doc.Roles, 1)
	require.Equal(t, "acme/viewer", doc.Roles[0].Key())
	require.Equal(t, "documents", doc.Roles[0].Scopes[0].Kind)

	require.Contains(t, reader.ConfigurationInformationLocalReader.AmbiguousWorkspaceConfigurations(), fixturefragments.Group,
		"the premise: core reports the second provider as an ambiguity, which is what this resolves")
	require.NotContains(t, reader.AmbiguousWorkspaceConfigurations(), fixturefragments.Group,
		"a group this reader supplies is not ambiguous")
}

// A workspace that answers for itself keeps its answer: `codefly generate
// fixture-composition` writes that file and a run writes it before it loads
// any configuration, and core's precedence then applies untouched.
func TestTheWorkspacesOwnDocumentWins(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	own := fragmentHead + `roles:
  - organization: acme
    name: admin
    scopes:
      - {kind: documents.collection, actions: [read]}
`
	reader := readerFor(t, fragmentWorkspace(t, own))
	groups := fixtureGroups(reader)
	require.Len(t, groups, 1)
	require.Equal(t, own, groups[0], "the workspace's own document is what is provided")
	require.NotContains(t, reader.AmbiguousWorkspaceConfigurations(), fixturefragments.Group)
}

// Every other workspace configuration keeps the single-provider rule: two
// modules defining one group differently stays ambiguous, through this reader
// as through core.
func TestAnOrdinaryGroupTwoModulesDisagreeOnStaysAmbiguous(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	workspace := fragmentWorkspace(t, "")
	modules, err := workspace.LoadModules(context.Background())
	require.NoError(t, err)
	for i, module := range modules {
		require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), "configurations/local/engine.env"),
			[]byte("MODE=m"+string(rune('a'+i))+"\n"), 0o600))
	}
	reader := readerFor(t, workspace)
	require.Contains(t, reader.AmbiguousWorkspaceConfigurations(), "engine")
	require.NotContains(t, reader.AmbiguousWorkspaceConfigurations(), fixturefragments.Group)
	require.ErrorContains(t, reader.AmbiguousWorkspaceConfigurations()["engine"], "provided by composed modules")
}

func TestIdenticalFragmentsMustStillConflict(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	ws := fragmentWorkspace(t, "")
	modules, err := ws.LoadModules(context.Background())
	require.NoError(t, err)
	var doc []byte
	for _, m := range modules {
		if m.Name == "documents" {
			doc, err = os.ReadFile(filepath.Join(m.Dir(), "configurations/local/fixture-composition.yaml"))
			require.NoError(t, err)
		}
	}
	for _, m := range modules {
		require.NoError(t, os.WriteFile(filepath.Join(m.Dir(), "configurations/local/fixture-composition.yaml"), doc, 0600))
	}
	_, err = fixturefragments.Prepare(context.Background(), ws, &environments.Environment{Name: "local"})
	require.ErrorIs(t, err, fixturecomposition.ErrConflict, "run generation rejects the duplicate")
	reader, err := NewWorkspaceConfigurationReader(context.Background(), ws)
	require.NoError(t, err)
	require.ErrorIs(t, reader.Load(context.Background(), resources.LocalEnvironment()), fixturecomposition.ErrConflict, "read-only consumers must reject the same duplicate")
}

func TestLocalFragmentMustJoinForeignFragment(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	external := fragmentWorkspace(t, "")
	modules, err := external.LoadModules(context.Background())
	require.NoError(t, err)
	var authority string
	for _, m := range modules {
		if m.Name == "authority" {
			authority = m.Dir()
		}
	}
	ws := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml":                                        "name: mixed\nlayout: modules\nmodules:\n - name: authority\n   path: " + authority + "\n - name: product\n",
		"modules/product/module.codefly.yaml":                           "name: product\nservices: []\n",
		"modules/product/configurations/local/fixture-composition.yaml": fragmentHead + "roles: [{organization: acme, name: viewer, scopes: [{kind: product, actions: [read]}]}]\n",
	})
	prepared, err := fixturefragments.Prepare(context.Background(), ws, &environments.Environment{Name: "local"})
	require.NoError(t, err)
	expected, err := fixturecomposition.Decode(prepared.Content)
	require.NoError(t, err)
	require.Len(t, expected.Roles, 1)
	groups := fixtureGroups(readerFor(t, ws))
	require.Len(t, groups, 1)
	got, err := fixturecomposition.Decode([]byte(groups[0]))
	require.NoError(t, err)
	require.Equal(t, expected, got, "run and read-only consumers must see the same merged grants")
}

func TestNoFragmentsMatchesGeneratedEmptyDocument(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	ws := fragmentWorkspace(t, "")
	modules, err := ws.LoadModules(context.Background())
	require.NoError(t, err)
	for _, m := range modules {
		require.NoError(t, os.Remove(filepath.Join(m.Dir(), "configurations/local/fixture-composition.yaml")))
	}
	prepared, err := fixturefragments.Prepare(context.Background(), ws, &environments.Environment{Name: "local"})
	require.NoError(t, err)
	expected, err := fixturecomposition.Decode(prepared.Content)
	require.NoError(t, err)
	groups := fixtureGroups(readerFor(t, ws))
	require.Len(t, groups, 1, "run generation supplies an empty valid document, so read-only consumers must too")
	got, err := fixturecomposition.Decode([]byte(groups[0]))
	require.NoError(t, err)
	require.Equal(t, expected, got)
}

// A structured workspace override still belongs to the module for Core's
// delivery scoping; that must not cause the fixture provider to replace it.
func TestWorkspaceDocumentOverridesAnUnambiguousModuleFragment(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	own := fragmentHead + "roles: [{organization: acme, name: admin, scopes: [{kind: product, actions: [read]}]}]\n"
	ws := fragmentWorkspace(t, own)
	modules, err := ws.LoadModules(context.Background())
	require.NoError(t, err)
	for _, module := range modules {
		require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), "configurations/local/fixture-composition.yaml"), []byte(fragmentHead), 0o600))
	}
	reader := readerFor(t, ws)
	require.NotContains(t, reader.CompositionRootWorkspaceConfigurationNames(), fixturefragments.Group, "Core retains the composed delivery scope")
	require.Equal(t, []string{own}, fixtureGroups(reader))
}
