package gitops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/stretchr/testify/require"
)

func TestRefreshDeliveredTelemetryKeepsServiceSnapshot(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "tree")
	opts := digestRender()
	opts.Destination = dest
	cm := filepath.Join("services", "api", "overlays", "prod", "configmap.yaml")
	res, err := RenderOwnedTree(t.Context(), opts, func(_ context.Context, root string) error {
		digestUnit(t, root, "enabled", "shop-api")
		f, err := os.OpenFile(filepath.Join(root, cm), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, err = f.WriteString("  CODEFLY__SERVICE: api\n  OTEL_EXPORTER_OTLP_ENDPOINT: https://previous.example:443\n  OTEL_RESOURCE_ATTRIBUTES: deployment.environment.name=prod\n  OTEL_SERVICE_NAME: shop/api\n  CODEFLY__WORKSPACE_CONFIGURATION__TRACE_EXPORT__OTEL_EXPORTER_OTLP_ENDPOINT: https://application.example:443\n")
		return errors.Join(err, f.Close())
	})
	require.NoError(t, err)
	env := &environments.Environment{Name: "prod", Telemetry: &environments.EnvironmentTelemetry{OTLPEndpoint: "https://new.example:443"}}
	snapshot := res.Inventory
	files := append([]InventoryFile(nil), snapshot.Files...)
	require.NoError(t, refreshDeliveredTelemetry(t.Context(), dest, env, &snapshot))
	require.Equal(t, res.Inventory.Pin, snapshot.Pin)
	var changed []string
	for idx, file := range files {
		if file.SHA256 != snapshot.Files[idx].SHA256 {
			changed = append(changed, file.Path)
		}
	}
	require.ElementsMatch(t, []string{cm, filepath.Join(filepath.Dir(cm), "kustomization.yaml")}, changed)
	content, err := os.ReadFile(filepath.Join(dest, cm))
	require.NoError(t, err)
	require.Contains(t, string(content), "https://new.example:443")
	require.Contains(t, string(content), "https://application.example:443")
	require.False(t, bytes.Contains(content, []byte("previous.example")))
	require.ErrorIs(t, refreshDeliveredTelemetry(t.Context(), dest, env, &snapshot), ErrNoChanges)
}

func TestRefreshRefusesChangingAnotherServiceIdentity(t *testing.T) {
	root := t.TempDir()
	digestUnit(t, root, "enabled", "shop-api")
	unitDir := filepath.Join(root, "services", "api")
	config := filepath.Join(unitDir, "overlays", "prod", "configmap.yaml")
	out, err := os.OpenFile(config, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = out.WriteString("  CODEFLY__SERVICE: api\n  OTEL_EXPORTER_OTLP_ENDPOINT: https://previous.example:443\n  OTEL_SERVICE_NAME: different/api\n  OTEL_RESOURCE_ATTRIBUTES: deployment.environment.name=prod\n")
	require.NoError(t, err)
	require.NoError(t, out.Close())
	before, err := os.ReadFile(config)
	require.NoError(t, err)
	env := &environments.Environment{Name: "prod", Telemetry: &environments.EnvironmentTelemetry{OTLPEndpoint: "https://next.example:443"}}
	updated, err := refreshServiceTelemetry(t.Context(), testRenderRoot(t, root), unitDir, env, "shop", "api")
	require.False(t, updated)
	require.ErrorContains(t, err, "differs from delivered service identity")
	after, err := os.ReadFile(config)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
