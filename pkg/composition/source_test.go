package composition

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestServiceUsesSourceRetainsMaterializationProvenance(t *testing.T) {
	for _, tc := range []struct {
		name, overlay, receipt, serviceReceipt, version string
		err                                             string
		mode                                            ResolutionMode
		source                                          bool
	}{
		{name: "in repository", source: true},
		{name: "unmaterialized package", version: "1.0.0", err: "unresolved or stale"},
		{name: "developer path", version: "1.0.0", overlay: "    path: ./checkout\n", source: true},
		{name: "materialized package", version: "1.0.0", overlay: "    path: ./checkout\n", receipt: "./checkout", mode: ResolutionModeVerified},
		{name: "materialized git", version: "1.0.0", overlay: "    path: ./checkout\n", receipt: "./checkout", mode: ResolutionModeGit, source: true},
		{name: "commit clone", version: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", overlay: "    path: ./checkout\n", receipt: "./checkout", mode: ResolutionModeCommit, source: true},
		{name: "local service in package", version: "1.0.0", overlay: "    path: ./checkout\n    services:\n      api:\n        path: ./editing-api\n", receipt: "./checkout", mode: ResolutionModeVerified, source: true},
		{name: "pinned service in local module", version: "1.0.0", overlay: "    path: ./checkout\n    services:\n      api:\n        version: 2.0.0\n", err: "unresolved or stale"},
		{name: "materialized service in local module", version: "1.0.0", overlay: "    path: ./checkout\n    services:\n      api:\n        path: ./package-api\n", serviceReceipt: "./package-api", mode: ResolutionModeVerified},
		{name: "removed declared git cannot become verified", version: "1.0.0", overlay: "    path: ./checkout\n", receipt: "./checkout", mode: ResolutionModeDeclaredGit, err: "unresolved or stale"},
		{name: "old receipt does not own new path", version: "1.0.0", overlay: "    path: ./editing\n", receipt: "./checkout", mode: ResolutionModeVerified, source: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
			dir := t.TempDir()
			ref := &resources.ModuleReference{Name: "shop", Version: tc.version}
			if tc.version != "" {
				ref.Source = "github.com/example/shop"
			}
			workspace := &resources.Workspace{Name: "test", Modules: []*resources.ModuleReference{ref}}
			workspace.WithDir(dir)
			require.NoError(t, os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte("name: test\n"), 0644))
			if tc.overlay != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, resources.LocalOverlayConfigurationName), []byte("resolve:\n  shop:\n"+tc.overlay), 0644))
			}
			receipts := map[string]*ResolutionReceipt{}
			if tc.receipt != "" {
				receipts["shop"] = &ResolutionReceipt{Path: tc.receipt, Mode: tc.mode, Source: ref.Source, Requested: tc.version, Version: tc.version}
			}
			if tc.serviceReceipt != "" {
				receipts["shop/api"] = &ResolutionReceipt{Path: tc.serviceReceipt, Mode: tc.mode, Service: "api", Source: ref.Source, Requested: "2.0.0", Version: "2.0.0"}
			}
			require.NoError(t, SaveResolutionReceipts(t.Context(), dir, receipts))
			got, err := ServiceUsesSource(t.Context(), workspace, "shop", "api")
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.source, got)
		})
	}
}
