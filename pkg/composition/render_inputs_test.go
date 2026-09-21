package composition

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/codefly-dev/core/artifactexecution"
	"github.com/stretchr/testify/require"
)

func TestRenderConfigurationBindsTypedPayloadsWithoutJSONOrdering(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	inputs := []RenderInput{{Target: "left", Service: "api", Protocol: artifactexecution.SolutionRender, Request: json.RawMessage(`{"values":{"a":"secret","b":"value"}}`)}}
	first, err := RenderConfigurationIdentity(key, inputs)
	require.NoError(t, err)
	inputs[0].Request = json.RawMessage(`{ "values": { "b": "value", "a": "secret" } }`)
	second, err := RenderConfigurationIdentity(key, inputs)
	require.NoError(t, err)
	require.Equal(t, first, second)
	inputs[0].Target = "right"
	second, err = RenderConfigurationIdentity(key, inputs)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	inputs[0].Target = "left"
	inputs[0].Request = json.RawMessage(`{"values":{"a":"changed","b":"value"}}`)
	second, err = RenderConfigurationIdentity(key, inputs)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	_, err = RenderConfigurationIdentity(key, append(inputs, inputs[0]))
	require.ErrorContains(t, err, "duplicate")
}

func TestRenderPayloadCannotOverrideHostOwnedFields(t *testing.T) {
	for _, tc := range []struct{ protocol, payload string }{
		{artifactexecution.BuilderRender, `{"execution":{}}`},
		{artifactexecution.BuilderRender, `{"outputDirectory":"/tmp"}`},
		{artifactexecution.SolutionRender, `{"execution":{}}`},
		{artifactexecution.SolutionRender, `{"destination":"/tmp"}`},
		{artifactexecution.SolutionRender, `{"artifactReference":"other"}`},
		{artifactexecution.SolutionRender, `{"context":{"artifact":{}}}`},
		{artifactexecution.SolutionRender, `{"unknown":"secret"}`},
	} {
		_, err := RenderConfigurationIdentity(bytes.Repeat([]byte{1}, 32), []RenderInput{{Service: "api", Protocol: tc.protocol, Request: json.RawMessage(tc.payload)}})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
	}
}
