package environment

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShowAdmitsAndPrintsCLIEnvironment(t *testing.T) {
	dir := writeWorkspace(t, "name: product\nlayout: modules\nenvironments:\n  - name: prod\n    namespace: target\n    cluster:\n      kind: gke\n      context: declared\n")
	t.Chdir(dir)
	prior := showJSON
	defer func() { showJSON = prior; showCmd.SetOut(nil) }()
	showJSON = true
	var out bytes.Buffer
	showCmd.SetOut(&out)
	require.NoError(t, showCmd.RunE(showCmd, []string{"prod"}))
	var document map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &document))
	require.Equal(t, "target", document["Namespace"])
	require.Equal(t, "declared", document["Cluster"].(map[string]any)["Context"])
	require.NotContains(t, document, "Extensions")
}
