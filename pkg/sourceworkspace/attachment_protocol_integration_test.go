//go:build integration

package sourceworkspace_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/internal/protocoltest"
	"github.com/codefly-dev/cli/pkg/sourceworkspace"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestSourceAttachmentPreservesSelectionAndBuilderIdentity(t *testing.T) {
	selection := protocoltest.Install(t, "attachment-peer")[0]
	defer services.ClearAgents()
	root := t.TempDir()
	module := filepath.Join(root, "module")
	require.NoError(t, os.MkdirAll(module, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.27.0\n\nuse ./module\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(module, "go.mod"), []byte("module example.test/attachment\n\ngo 1.27.0\n"), 0o600))
	selected, err := resources.ParseAgent(t.Context(), resources.ServiceAgent, selection)
	require.NoError(t, err)
	prepared, err := sourceworkspace.PrepareWithAgent(t.Context(), module, selected)
	require.NoError(t, err)
	defer prepared.Close()
	require.Equal(t, selected.Version, prepared.Service.Agent.Version)
	require.NotEmpty(t, prepared.GoWorkFile)
	require.Equal(t, true, prepared.Service.Spec["with-workspace"])
	instance, err := services.Load(t.Context(), prepared.Workspace, prepared.Module, prepared.Service)
	require.NoError(t, err)
	require.NoError(t, instance.LoadBuilder(t.Context()))
	_, err = instance.Builder.Load(t.Context())
	require.NoError(t, err)
	var loaded bool
	for _, call := range protocoltest.Calls(t, module) {
		if call.Method != "Builder.Load" {
			continue
		}
		request := &builderv0.LoadRequest{}
		require.NoError(t, protojson.Unmarshal(call.Request, request))
		require.Equal(t, instance.Identity.Name, request.GetIdentity().GetName())
		require.Equal(t, instance.Identity.Module, request.GetIdentity().GetModule())
		loaded = true
	}
	require.True(t, loaded, "prepared identity must reach the selected peer")
}
