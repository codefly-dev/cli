package environment

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestImagePolicyImportReplacesWithdrawnSelections(t *testing.T) {
	dir := writeWorkspace(t, "name: example\nenvironments:\n  - name: stage\n    build-images:\n      registries:\n        docker.io: old.example/cache\n      types:\n        codefly.dev/go:\n          runtime: {reference: scratch, variant: scratch}\n")
	for _, file := range []string{`services: {m/s: {runtime: {reference: scratch, variant: scratch}}}`, `{}`} {
		policy, err := parseImagePolicy([]byte(file))
		require.NoError(t, err)
		contract, err := json.Marshal(map[string]any{"schema": "codefly/coordinate/v1", "environment": map[string]any{"name": "stage", "build-images": policy}})
		require.NoError(t, err)
		var output bytes.Buffer
		opts := &importOptions{dir: dir, envName: "stage", contractData: contract, now: time.Now(), stdout: &output, dryRun: true}
		path := filepath.Join(dir, resources.WorkspaceConfigurationName)
		before, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, runImport(context.Background(), opts))
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, before, after)
		opts.dryRun = false
		require.NoError(t, runImport(context.Background(), opts))
		after, err = os.ReadFile(path)
		require.NoError(t, err)
		require.NotContains(t, string(after), "old.example")
		require.NotContains(t, string(after), "codefly.dev/go")
	}
	for _, bad := range []string{"null", "types: {}\n---\ntypes: {}", "typo: {}"} {
		_, err := parseImagePolicy([]byte(bad))
		require.Error(t, err)
	}
	require.Contains(t, imagesCmd.Long, "does not build or deploy")
}
