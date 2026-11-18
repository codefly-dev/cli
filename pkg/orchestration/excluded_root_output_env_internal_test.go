package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const excludedRootExportWorkspace = `name: excluded-export
layout: modules
modules:
    - name: quill
`

// gitops.WorkloadTokenFileVariable, which this package cannot import: the
// variable the launcher projects a vouched-for workload's credential under.
const workloadTokenFileVariable = "CODEFLY__WORKLOAD_TOKEN_FILE" //nolint:gosec // G101: the name of a path variable, not a credential

const excludedRootExportModule = `kind: module
name: quill
service-entry: backend
services:
    - name: backend
`

const excludedRootExportService = `kind: service
name: backend
version: 0.0.0
module: quill
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
		"workspace.codefly.yaml":                              excludedRootExportWorkspace,
		"modules/quill/module.codefly.yaml":                   excludedRootExportModule,
		"modules/quill/services/backend/service.codefly.yaml": excludedRootExportService,
	})

	module, err := workspace.LoadModuleFromName(ctx, "quill")
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
			"CODEFLY__API_CONSUMES":           `[{"id":"documents"}]`,
			"CODEFLY__MODULE_IDENTITY_SECRET": "documents:s3cr3t",
		},
	})

	require.NoError(t, flow.exportExcludedOriginEnvironment(ctx))

	body, err := os.ReadFile(output)
	require.NoError(t, err)
	require.Contains(t, string(body), `CODEFLY__API_CONSUMES=[{"id":"documents"}]`)
	require.Contains(t, string(body), "CODEFLY__MODULE_IDENTITY_SECRET=documents:s3cr3t")
}

// An override aimed at another service must not ride the root's export: the
// file is the root's own environment, and a secret minted for a consumed module
// belongs only to that module's processes.
func TestExcludedRootExportCarriesOnlyItsOwnOverrides(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml":                              excludedRootExportWorkspace,
		"modules/quill/module.codefly.yaml":                   excludedRootExportModule,
		"modules/quill/services/backend/service.codefly.yaml": excludedRootExportService,
	})

	module, err := workspace.LoadModuleFromName(ctx, "quill")
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
		"documents/api": {"CODEFLY__MODULE_IDENTITY_SECRET": "not-the-roots"},
	})

	require.NoError(t, flow.exportExcludedOriginEnvironment(ctx))

	body, err := os.ReadFile(output)
	require.NoError(t, err)
	require.NotContains(t, string(body), "not-the-roots")
}

func TestExcludedIdentityRootIsDeclaredWithoutStartingOrReadyEndpoints(t *testing.T) {
	ctx := t.Context()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml":                              excludedRootExportWorkspace,
		"modules/quill/module.codefly.yaml":                   excludedRootExportModule,
		"modules/quill/services/backend/service.codefly.yaml": excludedRootExportService,
	})
	module, err := workspace.LoadModuleFromName(ctx, "quill")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "backend")
	require.NoError(t, err)
	service.ModuleIdentity = true
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	flow, err := NewFlow(ctx, workspace, module, service, env, RunMode)
	require.NoError(t, err)
	flow.WithExcludeRoot(true)
	// The environment declares the host whose authority admits this root. The
	// run that cannot vouch for the root to it hands the root nothing.
	env.Host = &environments.EnvironmentHost{TrustDomain: "authority.local", Audience: "https://authority.local.test"}
	flow.publishRuntimeServices()
	require.Empty(t, flow.LocalRuntimeServices())
	require.Equal(t, []string{"quill/backend"}, flow.LocalAuthorityServices())
	require.ErrorContains(t, flow.prepareExcludedOrigin(ctx), "no workload authority")
	require.False(t, flow.externalPrepared.Load())
	calls := 0
	flow.world.PrepareExternalWorkload = func(_ context.Context, selected *resources.Service) (func(), error) {
		require.Same(t, service, selected)
		calls++
		return func() {}, nil
	}
	require.NoError(t, flow.prepareExcludedOrigin(ctx))
	require.Equal(t, 1, calls)
	require.Empty(t, flow.LocalRuntimeServices(), "admission is not an agent Start")
	service.ModuleIdentity = false
	flow.publishRuntimeServices()
	require.Empty(t, flow.LocalAuthorityServices())
	require.NoError(t, flow.prepareExcludedOrigin(ctx))
	require.Equal(t, 1, calls, "plain excluded roots acquire no authority")
	service.ModuleIdentity = true
	abandoned := false
	flow.world.PrepareExternalWorkload = func(context.Context, *resources.Service) (func(), error) {
		return func() { abandoned = true }, nil
	}
	flow.WithRuntimeContext(resources.RuntimeContextNative)
	require.NoError(t, flow.ConfigurationManager.Load(ctx, env.Runtime()))
	flow.WithOutputEnv(t.TempDir()) // a directory cannot receive the environment file
	require.Error(t, flow.prepareExcludedOrigin(ctx))
	require.True(t, abandoned, "an environment export failure abandons the prepared projection")
	require.False(t, flow.externalPrepared.Load())
}

// A composition whose environment declares no host has no authority a workload
// could be admitted by: the root's module-identity is a request nothing in
// this composition answers. Its dependency run is the run a started root would
// get there, with every other carrier and no identity. A suite that boots its
// dependencies in the module's own workspace is this run.
func TestExcludedIdentityRootOfAHostlessCompositionIsExportedWithoutAnIdentity(t *testing.T) {
	ctx := t.Context()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml":                              excludedRootExportWorkspace,
		"modules/quill/module.codefly.yaml":                   excludedRootExportModule,
		"modules/quill/services/backend/service.codefly.yaml": excludedRootExportService,
	})
	module, err := workspace.LoadModuleFromName(ctx, "quill")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "backend")
	require.NoError(t, err)
	service.ModuleIdentity = true
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	require.Nil(t, env.Host)
	flow, err := NewFlow(ctx, workspace, module, service, env, RunMode)
	require.NoError(t, err)

	output := filepath.Join(t.TempDir(), "runtime.env")
	flow.WithRuntimeContext(resources.RuntimeContextNative)
	flow.WithExcludeRoot(true)
	flow.WithOutputEnv(output)
	flow.WithOverrides(map[string]map[string]string{
		resources.WithUnique(service).Unique(): {"CODEFLY__API_CONSUMES": `[{"id":"documents"}]`},
	})
	flow.publishRuntimeServices()
	require.Empty(t, flow.LocalRuntimeServices())
	require.Empty(t, flow.LocalAuthorityServices(), "no authority is asked to admit the root")

	require.NoError(t, flow.prepareExcludedOrigin(ctx))
	require.True(t, flow.externalPrepared.Load())
	require.NotContains(t, flow.DependencyProcessVariables(service), workloadTokenFileVariable)
	body, err := os.ReadFile(output)
	require.NoError(t, err)
	require.Contains(t, string(body), `CODEFLY__API_CONSUMES=[{"id":"documents"}]`)
	require.NotContains(t, string(body), workloadTokenFileVariable)
	flow.externalPrepared.Store(false)
	if failure := flow.Readiness(ctx); failure != nil {
		require.NotEqual(t, PredicateExternalIdentity, failure.Predicate, "nothing waits on an admission no authority performs")
	}
}
