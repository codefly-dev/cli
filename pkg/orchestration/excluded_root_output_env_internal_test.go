package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const excludedRootExportWorkspace = `name: excluded-export
layout: modules
modules:
    - name: wiki
`

const excludedRootExportModule = `kind: module
name: wiki
service-entry: backend
services:
    - name: backend
`

const excludedRootExportService = `kind: service
name: backend
version: 0.0.0
module: wiki
agent:
    kind: runtime::service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
`

// An excluded root never starts, so this export is the only thing that can hand
// the process standing in for it the values the run derived. Both run paths set
// those values the same way — per-service overrides keyed by the
// module-qualified unique — so this is the seam the whole --exclude-root
// hand-off rests on, whichever entry point composed the flow.
//
// The keys must arrive under their own names: the solution runtimes read
// CODEFLY__API_CONSUMES verbatim, so a prefixed projection would be useless to
// them.
func TestExcludedRootExportCarriesDerivedOverrides(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml":                             excludedRootExportWorkspace,
		"modules/wiki/module.codefly.yaml":                   excludedRootExportModule,
		"modules/wiki/services/backend/service.codefly.yaml": excludedRootExportService,
	})

	module, err := workspace.LoadModuleFromName(ctx, "wiki")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "backend")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	flow, err := NewFlow(ctx, workspace, module, service, env, RunMode)
	require.NoError(t, err)

	output := filepath.Join(t.TempDir(), "runtime.env")
	flow.WithRuntimeContext(resources.RuntimeContextNative)
	flow.WithExcludeRoot(true)
	flow.WithOutputEnv(output)
	flow.WithOverrides(map[string]map[string]string{
		resources.WithUnique(service).Unique(): {
			"CODEFLY__API_CONSUMES":                `[{"id":"documents"}]`,
			"CODEFLY__MODULE_REGISTRATION_SECRETS": "documents:s3cr3t",
		},
	})

	require.NoError(t, flow.exportExcludedOriginEnvironment(ctx))

	body, err := os.ReadFile(output)
	require.NoError(t, err)
	require.Contains(t, string(body), `CODEFLY__API_CONSUMES=[{"id":"documents"}]`)
	require.Contains(t, string(body), "CODEFLY__MODULE_REGISTRATION_SECRETS=documents:s3cr3t")
}

// An override aimed at another service must not ride the root's export: the
// file is the root's own environment, and a secret minted for a consumed module
// belongs only to that module's processes.
func TestExcludedRootExportCarriesOnlyItsOwnOverrides(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml":                             excludedRootExportWorkspace,
		"modules/wiki/module.codefly.yaml":                   excludedRootExportModule,
		"modules/wiki/services/backend/service.codefly.yaml": excludedRootExportService,
	})

	module, err := workspace.LoadModuleFromName(ctx, "wiki")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "backend")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	flow, err := NewFlow(ctx, workspace, module, service, env, RunMode)
	require.NoError(t, err)

	output := filepath.Join(t.TempDir(), "runtime.env")
	flow.WithRuntimeContext(resources.RuntimeContextNative)
	flow.WithExcludeRoot(true)
	flow.WithOutputEnv(output)
	flow.WithOverrides(map[string]map[string]string{
		"documents/api": {"CODEFLY__MODULE_REGISTRATION_SECRET": "not-the-roots"},
	})

	require.NoError(t, flow.exportExcludedOriginEnvironment(ctx))

	body, err := os.ReadFile(output)
	require.NoError(t, err)
	require.NotContains(t, string(body), "not-the-roots")
}
