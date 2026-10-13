package composition

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/artifactexecution"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	solutionv0 "github.com/codefly-dev/core/generated/go/codefly/services/solution/v0"
	"github.com/stretchr/testify/require"
)

func TestRenderProvenanceIsHostOwned(t *testing.T) {
	for _, payload := range []string{
		`{"compositionProvenance":{}}`,
		`{"compositionProvenance":{"members":[{"name":"saas","role":"ROLE_MODULE","workspace":"wrong"}]}}`,
	} {
		_, err := decodeRenderInputs([]RenderInput{{Target: "modules/saas", Service: "accounts", Protocol: artifactexecution.BuilderRender, Request: json.RawMessage(payload)}})
		require.ErrorContains(t, err, "composition provenance are host-owned")
	}
}

func TestEngineRenderBindsOneLoadedCompositionToEveryBuilder(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		path = filepath.Join(root, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	write("platform/workspace.codefly.yaml", "name: platform-core\nlayout: modules\nmodules:\n  - name: saas\n    source: github.com/codefly-dev/module-saas-starter\n    version: v0.0.93\n    module: module\n")
	write("product/workspace.codefly.yaml", "name: platform-obin\nlayout: modules\nworkspaces:\n  - name: platform-core\n    path: ../platform\nsolutions:\n  - name: lastlogin-go\n    path: solutions/lastlogin-go\n")
	write("product/solutions/lastlogin-go/module.codefly.yaml", "kind: module\nname: lastlogin-go\nservices: []\n")
	session := &SelectionSession{trustPath: filepath.Join(root, "product/workspace.codefly.yaml")}
	first, second := &builderv0.DeploymentRequest{}, &builderv0.DeploymentRequest{}
	require.NoError(t, session.bindRenderProvenance(t.Context(), []renderInput{
		{request: first}, {request: &solutionv0.RenderRequest{}}, {request: second},
	}))
	require.Equal(t, []*builderv0.CompositionMember{
		{Name: "saas", Role: builderv0.CompositionMember_ROLE_MODULE, Workspace: "platform-core"},
		{Name: "lastlogin-go", Role: builderv0.CompositionMember_ROLE_SOLUTION, Workspace: "platform-obin"},
	}, first.GetCompositionProvenance().GetMembers())
	require.Equal(t, first.GetCompositionProvenance(), second.GetCompositionProvenance())
	first.CompositionProvenance.Members[0].Name = "mutated"
	require.Equal(t, "saas", second.CompositionProvenance.Members[0].Name, "requests must not share mutable membership")
}

func TestStageRenderRefusesPeerWithoutCompositionProvenance(t *testing.T) {
	session, files, options := stageFixtureWithSelection(t, []string{"left"}, []string{"builder"})
	options.LoadOptions = func(directory string) ([]manager.LoadOption, error) {
		opts, err := stageTestOptions(directory)
		return append(opts, manager.WithEnv("RENDER_TEST_MODE=missing-provenance")), err
	}
	result, err := session.StageRender(t.Context(), files, options)
	require.Nil(t, result)
	require.ErrorContains(t, err, contract.DeploymentCompositionProvenance)
}
