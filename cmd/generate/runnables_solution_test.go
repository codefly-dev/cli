package generate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runnablev0 "github.com/codefly-dev/core/generated/go/codefly/runnable/v0"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/stretchr/testify/require"
)

// releasesRoute is one route of the releases solution's backend as its
// runtime publishes it: the policy it declares and, for a read, the marker
// core writes from that policy (corerunnable.RouteMarker), which is exactly
// what solution-runtime-go puts beside it.
type releasesRoute struct {
	method, path, id, summary, response string
	policy                              string
	query                               []map[string]any
}

var releasesRoutes = []releasesRoute{
	{method: "get", path: "/v1/releases/recorded", id: "Releases_Get_V1_Releases_Recorded",
		summary: "Read the board as records holds it, without asking GitHub", response: "Board",
		policy: `{"requires":[{"resource_kind":"records.records","actions":["read"]}]}`},
	{method: "get", path: "/v1/releases/history", id: "Releases_Get_V1_Releases_History",
		summary: "Read the recorded tags of one repository and the recorded pins of one component", response: "History",
		policy: `{"requires":[{"resource_kind":"records.records","actions":["read"]}]}`,
		query: []map[string]any{
			{"name": "repo", "in": "query", "required": true, "type": "string"},
			{"name": "component", "in": "query", "required": false, "type": "string"},
		}},
	{method: "post", path: "/v1/releases/record", id: "Releases_Post_V1_Releases_Record",
		summary: "Record every tag and pin the board shows that is not recorded yet", response: "RecordResult",
		policy: `{"requires":[{"resource_kind":"records.records","actions":["read","write"]}]}`},
	{method: "post", path: "/v1/releases/chat/install", id: "Releases_Post_V1_Releases_Chat_Install",
		summary: "Import this solution's Robin intent into the installation", response: "RecordResult",
		policy: `{"requires":[{"resource_kind":"robin.intents","actions":["import","activate"]}]}`},
	{method: "get", path: "/v1/releases/status", id: "Releases_Get_V1_Releases_Status",
		summary: "Report whether the viewer's GitHub source is connected", response: "RecordResult",
		policy: `{"any_checked_caller":true}`},
}

// releasesDocument is the releases backend's Swagger 2.0 artifact. edit
// reshapes the parsed document for the case under test.
func releasesDocument(t *testing.T, edit func(map[string]any)) []byte {
	t.Helper()
	object := func(properties map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	}
	paths := map[string]any{}
	for _, route := range releasesRoutes {
		operation := map[string]any{
			"operationId": route.id,
			"summary":     route.summary,
			"responses": map[string]any{
				"200": map[string]any{"description": "A successful response.", "schema": map[string]any{"$ref": "#/definitions/" + route.response}},
				"401": map[string]any{"description": "No Work Context.", "schema": map[string]any{"$ref": "#/definitions/Error"}},
			},
			"x-codefly-authority-policy": json.RawMessage(route.policy),
		}
		if len(route.query) > 0 {
			operation["parameters"] = route.query
		}
		if route.method == "get" && strings.Contains(route.policy, `["read"]`) {
			marker, err := corerunnable.RouteMarker(route.method, route.path, json.RawMessage(route.policy), "releases", route.id, route.summary)
			require.NoError(t, err)
			operation[corerunnable.OpenAPIOperationMarker] = marker
		}
		paths[route.path] = map[string]any{route.method: operation}
	}
	raw, err := json.Marshal(map[string]any{
		"swagger":  "2.0",
		"info":     map[string]any{"title": "releases solution backend", "version": "0.3.0"},
		"consumes": []string{"application/json"},
		"produces": []string{"application/json"},
		"paths":    paths,
		"definitions": map[string]any{
			"Board":        object(map[string]any{"observedAt": map[string]any{"type": "string"}}),
			"History":      object(map[string]any{"repo": map[string]any{"type": "string"}}),
			"RecordResult": object(map[string]any{"tags": map[string]any{"type": "integer"}}),
			"Error":        object(map[string]any{"error": map[string]any{"type": "string"}}),
		},
	})
	require.NoError(t, err)
	if edit == nil {
		return raw
	}
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	edit(doc)
	raw, err = json.Marshal(doc)
	require.NoError(t, err)
	return raw
}

// saveSolutionFixture is a workspace holding the releases solution: a module
// whose backend serves its routes on an http endpoint and publishes them as
// OpenAPI, the way `codefly generate contracts` leaves a solution behind.
func saveSolutionFixture(t *testing.T, ctx context.Context, document []byte) (root, moduleDir string) {
	t.Helper()
	root, moduleDir = saveFixtureWorkspace(t, ctx, "releases",
		&resources.ModuleInterface{Endpoints: []*resources.InterfaceEndpoint{
			{Service: "backend", Endpoint: "http", Visibility: resources.VisibilityPublic},
		}},
		&resources.Service{
			Name:    "backend",
			Version: "0.3.0",
			Agent:   &resources.Agent{Kind: resources.ServiceAgent, Name: "go", Version: "0.0.70", Publisher: "codefly.dev"},
			Endpoints: []*resources.Endpoint{
				{Name: "http", API: "http", Visibility: resources.VisibilityPublic, Exposure: resources.ExposureNone},
			},
		},
	)
	publishSolutionContract(t, moduleDir, document)
	return root, moduleDir
}

func publishSolutionContract(t *testing.T, moduleDir string, document []byte) {
	t.Helper()
	const contractPath = "contracts/api/backend/http/openapi.json"
	writeFixtureFile(t, filepath.Join(moduleDir, filepath.FromSlash(contractPath)), document)
	routes := make([]composition.APIContractRoute, 0, len(releasesRoutes))
	for _, route := range releasesRoutes {
		routes = append(routes, composition.APIContractRoute{Method: strings.ToUpper(route.method), Path: route.path})
	}
	catalog := &composition.APIContractCatalog{
		Schema:  composition.APIContractCatalogSchema,
		Package: "obin/releases",
		Version: "0.3.0",
		Endpoints: []composition.APIContractEndpoint{{
			Service:  "backend",
			Endpoint: "http",
			API:      "http",
			Kind:     composition.APIContractKindOpenAPI,
			Package:  "releases solution backend",
			Path:     contractPath,
			Digest:   composition.APIContractDigest(document),
			Routes:   routes,
		}},
	}
	catalogBytes, err := catalog.CanonicalBytes()
	require.NoError(t, err)
	writeFixtureFile(t, filepath.Join(moduleDir, filepath.FromSlash(composition.APIContractCatalogFileName)), catalogBytes)
}

// TestGenerateRunnablesDerivesASolutionsReads is the solution half of "every
// Runnable is a tool": the same command, given a solution, writes the same
// tree from its OpenAPI document — one directory per marked route, named by
// its operationId — and a solution's two reads become read-only Runnables
// while its writes, which its runtime does not mark, become nothing.
func TestGenerateRunnablesDerivesASolutionsReads(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveSolutionFixture(t, ctx, releasesDocument(t, nil))
	t.Chdir(root)
	resetRunnablesFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"releases"}))

	require.Equal(t, []string{
		"backend/http/Releases_Get_V1_Releases_History/operation.json",
		"backend/http/Releases_Get_V1_Releases_History/runnable-package.json",
		"backend/http/Releases_Get_V1_Releases_Recorded/operation.json",
		"backend/http/Releases_Get_V1_Releases_Recorded/runnable-package.json",
		"index.json",
	}, generatedTree(t, moduleDir))

	index := readIndex(t, moduleDir)
	require.Len(t, index.Operations, 2)
	history := index.Operations[0]
	require.Equal(t, "backend-http-releases-get-v1-releases-history", history.Name)
	require.Equal(t, "GET /v1/releases/history", history.Method)
	require.Equal(t, "Releases_Get_V1_Releases_History", history.InputMessage)
	require.Equal(t, "History", history.OutputMessage)
	require.Equal(t, "contracts/runnables/backend/http/Releases_Get_V1_Releases_History", history.Path)

	pkg := readDerivedPackage(t, moduleDir, history)
	require.NoError(t, corerunnable.VerifyPackage(pkg))
	require.Equal(t, history.Digest, pkg.GetDigest())
	require.Equal(t, basev0.RunnableExecution_RECOVERY_RECOMPUTE, pkg.GetExecution().GetRecovery())
	require.Len(t, pkg.GetContract().GetInput().GetFields(), 2, "the query is the input")

	operation := readDerivedOperation(t, moduleDir, history)
	require.Equal(t, "releases", operation.Audience)
	require.Equal(t, []string{"503"}, operation.RetryableCodes)
	require.Equal(t, runnablev0.ToolExposure_EFFECT_READ_ONLY, operation.Tool.GetEffect())
	require.Equal(t, "Read the recorded tags of one repository and the recorded pins of one component", operation.Tool.GetDescription())

	// Idempotent: a second run is the same tree.
	before := snapshotTree(t, moduleDir)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"releases"}))
	require.True(t, equalTrees(before, snapshotTree(t, moduleDir)))
}

// TestGenerateRunnablesRefusesAMarkedRouteCoreRefuses: a marker is the owner's
// declaration, so a marked GET that would mutate is refused by name, and the
// command fails and writes nothing — never a write tool over GET.
func TestGenerateRunnablesRefusesAMarkedRouteCoreRefuses(t *testing.T) {
	ctx := context.Background()
	document := releasesDocument(t, func(doc map[string]any) {
		marker, err := corerunnable.RouteMarker("POST", "/v1/releases/record",
			json.RawMessage(`{"requires":[{"resource_kind":"records.records","actions":["read","write"]}]}`), "releases", "record", "Record")
		require.NoError(t, err)
		var declared map[string]any
		require.NoError(t, json.Unmarshal(marker, &declared))
		history := doc["paths"].(map[string]any)["/v1/releases/history"].(map[string]any)["get"].(map[string]any)
		history[corerunnable.OpenAPIOperationMarker] = declared
	})
	root, moduleDir := saveSolutionFixture(t, ctx, document)
	t.Chdir(root)
	resetRunnablesFlags(t)
	err := RunnablesCmd.RunE(RunnablesCmd, []string{"releases"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "GET /v1/releases/history")
	require.Contains(t, err.Error(), "admitted only as a read")
	_, statErr := os.Stat(filepath.Join(moduleDir, "contracts", "runnables"))
	require.True(t, os.IsNotExist(statErr), "a refused run wrote the tree")
}

// TestGenerateRunnableBindingsPreparesASolutionsReads: each derived read is
// prepared keyed SOLUTION__OPERATION, as a module's is MODULE__OPERATION, and
// calls the route itself — the verb, the path and the http endpoint's
// resolved address — under the policy core derived for it.
func TestGenerateRunnableBindingsPreparesASolutionsReads(t *testing.T) {
	ctx := context.Background()
	root, _ := saveSolutionFixture(t, ctx, releasesDocument(t, nil))
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"releases"}))
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))

	raw, err := os.ReadFile(filepath.Join(root, "configurations", "local", RunnableBindingsGroup+".env"))
	require.NoError(t, err)
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if key, value, found := strings.Cut(line, "="); found && !strings.HasPrefix(line, "#") {
			values[key] = value
		}
	}
	require.Len(t, values, 2, "the reads and nothing else:\n%s", raw)
	for key, route := range map[string]string{
		RunnableBindingKey("releases", "backend-http-releases-get-v1-releases-history"):  "/v1/releases/history",
		RunnableBindingKey("releases", "backend-http-releases-get-v1-releases-recorded"): "/v1/releases/recorded",
	} {
		require.True(t, strings.HasPrefix(key, "RELEASES__"), key)
		value, ok := values[key]
		require.True(t, ok, "no binding %s in:\n%s", key, raw)
		prepared, err := corerunnable.DecodePrepared([]byte(value))
		require.NoError(t, err)
		require.Equal(t, "GET", prepared.GetCall().GetRest().GetVerb())
		require.Equal(t, route, prepared.GetCall().GetRest().GetPath())
		require.True(t, strings.HasPrefix(prepared.GetCall().GetAddress(), "http://localhost:"), prepared.GetCall().GetAddress())
		tool, err := corerunnable.ToolFromPrepared(prepared)
		require.NoError(t, err)
		require.Equal(t, runnablev0.ToolExposure_EFFECT_READ_ONLY, tool.GetEffect())
		require.Equal(t, basev0.RunnableExecution_RECOVERY_RECOMPUTE, prepared.GetPolicy().GetRecovery())
	}
}
