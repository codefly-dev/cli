package orchestration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// edgeRouteWorkspace is rpcHopWorkspace with a browser edge beside the hop: a
// service declaring edge-routes, as the hop declares hop-routes.
func edgeRouteWorkspace(t *testing.T, services []string, foreign []string) *resources.Workspace {
	t.Helper()
	workspace := rpcHopWorkspace(t, services, foreign)
	for name, content := range map[string]string{
		"modules/authority/module.codefly.yaml":                "name: authority\nservices:\n - name: hop\n - name: edge\n",
		"modules/authority/services/edge/service.codefly.yaml": "name: edge\nversion: 0.0.0\nagent: {kind: runtime::service, name: go-grpc, version: 0.0.16, publisher: codefly.ai}\nworkspace-configuration-dependencies: [edge, edge-routes]\n",
	} {
		path := filepath.Join(workspace.Dir(), name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	reloaded, err := resources.LoadWorkspaceFromDir(context.Background(), workspace.Dir())
	require.NoError(t, err)
	return reloaded
}

// mounted is the composition's two authored lists: the modules the edge
// mounts and the modules published through the hop.
func mounted(edge, hop string) []*basev0.Configuration {
	return []*basev0.Configuration{{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{
		{Name: "edge", ConfigurationValues: []*basev0.ConfigurationValue{{Key: "MODULES", Value: edge}}},
		{Name: "hop", ConfigurationValues: []*basev0.ConfigurationValue{{Key: "MODULES", Value: hop}}},
	}}}
}

func edgeRoutesOf(t *testing.T, workspace *resources.Workspace, configs []*basev0.Configuration) (string, error) {
	t.Helper()
	group, err := generatedEdgeRoutes(context.Background(), workspace, configs)
	if err != nil {
		return "", err
	}
	require.NotNil(t, group)
	raw, delivered := groupValue([]*basev0.Configuration{group}, "edge-routes", "ROUTES")
	require.True(t, delivered)
	return raw, nil
}

// The edge routes what a module declares: every procedure of every service
// the module publishes on a Connect listener behind the hop, read from the
// catalog the hop's routes are read from, so no composition lists them. The
// document is held here byte for byte: the edge decodes exactly this
// (authority, modules/authority/services/edge/code/routes_test.go).
func TestEdgeRoutesAreTheProceduresAMountedModulePublishesOverConnect(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	raw, err := edgeRoutesOf(t, edgeRouteWorkspace(t, []string{"chat", "worker"}, nil), mounted("owner", "owner"))
	require.NoError(t, err)
	require.Equal(t, `{"schema":"codefly.dev/edge-routes/v1","modules":[{"audience":"owner","procedures":["/chat.v1.Service/Call","/worker.v1.Service/Call"]}]}`, raw)
}

// A foreign method is another module's protocol, served to that module and
// never to a page; two mounted modules may serve the same one, and a page's
// path names no module to choose between them. The hop routes it, the edge
// does not.
func TestEdgeRoutesLeaveOutForeignMethods(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	workspace := edgeRouteWorkspace(t, []string{"chat"}, []string{"/annotations.v1.TargetAuthorityService/ListKinds"})
	raw, err := edgeRoutesOf(t, workspace, mounted("owner", "owner"))
	require.NoError(t, err)
	require.Equal(t, `{"schema":"codefly.dev/edge-routes/v1","modules":[{"audience":"owner","procedures":["/chat.v1.Service/Call"]}]}`, raw)
	// The hop still routes it.
	routes, err := hopRoutesOf(t, workspace)
	require.NoError(t, err)
	require.Contains(t, routes.Endpoints[0].Methods, "/annotations.v1.TargetAuthorityService/ListKinds")
}

// A module the edge mounts that is not published through the hop — a
// solution's own backend, reached at its REST face — publishes nothing the
// edge routes by procedure, and needs no catalog. The group is still
// delivered, a document naming no module, so the declaration resolves and the
// edge can tell this CLI from one that generates nothing.
func TestEdgeRoutesNameOnlyModulesPublishedThroughTheHop(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	workspace := edgeRouteWorkspace(t, []string{"chat"}, nil)
	for name, configs := range map[string][]*basev0.Configuration{
		"mounted, not behind the hop": mounted("page, owner", ""),
		"nothing mounted":             mounted("", "owner"),
		"no edge group":               nil,
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := edgeRoutesOf(t, workspace, configs)
			require.NoError(t, err)
			require.Equal(t, `{"schema":"codefly.dev/edge-routes/v1","modules":[]}`, raw)
		})
	}
	raw, err := edgeRoutesOf(t, workspace, mounted("page,owner,,owner", "owner"))
	require.NoError(t, err)
	var doc struct {
		Modules []struct{ Audience string } `json:"modules"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &doc))
	require.Len(t, doc.Modules, 1)
	require.Equal(t, "owner", doc.Modules[0].Audience)
}

// The group is generated, never authored, and generated only for a service
// that declares it. hop-routes keeps its own rule: the edge's declaration of
// edge-routes makes it no hop, and the hop receives no edge routes.
func TestEdgeRoutesAreGeneratedForADeclaringServiceOnly(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	ctx := context.Background()
	authored := mounted("owner", "owner")
	authored[0].Infos = append(authored[0].Infos, &basev0.ConfigurationInformation{Name: "edge-routes", ConfigurationValues: []*basev0.ConfigurationValue{{Key: "ROUTES", Value: "{}"}}})
	_, err := generatedEdgeRoutes(ctx, edgeRouteWorkspace(t, []string{"chat"}, nil), authored)
	require.ErrorContains(t, err, "edge-routes is generated from published API catalogs; remove its authored configuration")

	// No service declares it: nothing is generated.
	group, err := generatedEdgeRoutes(ctx, rpcHopWorkspace(t, []string{"chat"}, nil), mounted("owner", "owner"))
	require.NoError(t, err)
	require.Nil(t, group)

	// A mounted module published through the hop that the composition does
	// not carry is refused by name.
	_, err = generatedEdgeRoutes(ctx, edgeRouteWorkspace(t, []string{"chat"}, nil), mounted("absent", "absent"))
	require.ErrorContains(t, err, "edge routes for absent")
}

// The reader delivers both generated groups, each to its own declarer's
// group name, from one load.
func TestTheReaderDeliversEdgeRoutesBesideHopRoutes(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	ctx := context.Background()
	workspace := edgeRouteWorkspace(t, []string{"chat"}, nil)
	for name, content := range map[string]string{
		"configurations/local/edge.env": "MODULES=owner\nOWNER_CONNECT=${endpoint:authority/hop/http}\n",
		"configurations/local/hop.env":  "MODULES=owner\n",
	} {
		path := filepath.Join(workspace.Dir(), name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	reader, err := NewWorkspaceConfigurationReader(ctx, workspace)
	require.NoError(t, err)
	require.NoError(t, reader.Load(ctx, resources.LocalEnvironment()))
	raw, delivered := groupValue(reader.Configurations(), "edge-routes", "ROUTES")
	require.True(t, delivered)
	require.Equal(t, `{"schema":"codefly.dev/edge-routes/v1","modules":[{"audience":"owner","procedures":["/chat.v1.Service/Call"]}]}`, raw)
	hop, delivered := groupValue(reader.Configurations(), hopRoutesGroup, hopRoutesKey)
	require.True(t, delivered)
	require.Contains(t, hop, "/chat.v1.Service/Call")
}
