package composition

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/codefly-dev/core/artifactexecution"
	"github.com/stretchr/testify/require"
)

func TestBuildAndRenderShareExactConfigurationIdentity(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	renders := []RenderInput{{Service: "api", Protocol: artifactexecution.BuilderRender, Request: json.RawMessage(`{}`)}}
	before, err := RenderConfigurationIdentity(key, renders)
	require.NoError(t, err)
	unchanged, err := ExecutionConfigurationIdentity(key, renders, nil)
	require.NoError(t, err)
	require.Equal(t, before, unchanged, "render-only identities remain stable")
	builds := []BuildInput{{Target: "left", Service: "api", Request: json.RawMessage(`{}`)}, {Target: "right", Service: "api", Request: json.RawMessage(`{}`)}}
	first, err := ExecutionConfigurationIdentity(key, renders, builds)
	require.NoError(t, err)
	require.NotEqual(t, before, first)
	builds[0], builds[1] = builds[1], builds[0]
	builds[0].Request = json.RawMessage(`{ "buildContext": {} }`)
	changed, err := ExecutionConfigurationIdentity(key, renders, builds)
	require.NoError(t, err)
	require.NotEqual(t, first, changed)
	builds[0].Request = json.RawMessage(`{ }`)
	reordered, err := ExecutionConfigurationIdentity(key, renders, builds)
	require.NoError(t, err)
	require.Equal(t, first, reordered)
	builds[0].Target = "other"
	changed, err = ExecutionConfigurationIdentity(key, renders, builds)
	require.NoError(t, err)
	require.NotEqual(t, first, changed)
}

func TestBuildPayloadRefusesHostOverridesAndDuplicateInstances(t *testing.T) {
	for _, payload := range []string{`{"execution":{}}`, `{"outputDirectory":"/tmp"}`, `{"unknown":"secret-value"}`} {
		_, err := decodeBuildInputs([]BuildInput{{Service: "api", Request: json.RawMessage(payload)}})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret-value")
	}
	_, err := decodeBuildInputs(nil)
	require.Error(t, err)
	input := BuildInput{Service: "api", Request: json.RawMessage(`{}`)}
	_, err = decodeBuildInputs([]BuildInput{input, input})
	require.ErrorContains(t, err, "duplicate")
}
