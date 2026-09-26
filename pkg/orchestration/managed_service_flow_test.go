package orchestration

import (
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// ambiguousManagedWorkspace composes two modules that each declare a service
// named "redis", with an environment replacing "redis" by a bare key — the
// declaration that cannot say which of the two is managed.
func ambiguousManagedWorkspace(t *testing.T, managedKey string) *resources.Workspace {
	t.Helper()
	files := map[string]string{
		"workspace.codefly.yaml": `name: platform
layout: modules
modules:
    - name: sessions
    - name: catalog
environments:
    - name: staging
      namespace: platform
      managed-services:
        ` + managedKey + `:
          kind: redis
          external-name: cache.internal.example
          port: 6379
`,
	}
	for _, module := range []string{"sessions", "catalog"} {
		files["modules/"+module+"/module.codefly.yaml"] = `kind: module
name: ` + module + `
services:
    - name: redis
`
		files["modules/"+module+"/services/redis/service.codefly.yaml"] = `kind: service
name: redis
version: 0.0.0
module: ` + module + `
agent:
    kind: runtime::service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
`
	}
	return writeTempWorkspace(t, files)
}

// A flow resolves managed services through env.ManagedService — the remote
// network manager asks it whether a service is replaced, and answers yes for both
// same-named services under a bare key, suppressing the in-cluster address of the
// one that is actually rendered. The gitops passes refuse that declaration; a run
// never reaches them, so NewFlow applies the same rule before it builds anything.
func TestNewFlowRefusesAnAmbiguousBareManagedService(t *testing.T) {
	ctx := t.Context()
	workspace := ambiguousManagedWorkspace(t, "redis")
	module, err := workspace.LoadModuleFromName(ctx, "sessions")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "redis")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, "staging")
	require.NoError(t, err)

	_, err = NewFlow(ctx, workspace, module, service, env, RunMode)
	require.ErrorContains(t, err, "ambiguous")
	require.ErrorContains(t, err, "sessions/redis")
	require.ErrorContains(t, err, "catalog/redis")
}

// The refusal is about the declaration, not about which modules this run starts:
// seeding the closure with one module does not make a workspace-wide ambiguity
// safe, because the entry still replaces both services.
func TestNewFlowRefusesAnAmbiguousBareManagedServiceOutsideTheRunClosure(t *testing.T) {
	ctx := t.Context()
	workspace := ambiguousManagedWorkspace(t, "redis")
	module, err := workspace.LoadModuleFromName(ctx, "sessions")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "redis")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, "staging")
	require.NoError(t, err)

	_, err = NewFlow(ctx, workspace, module, service, env, RunMode, WithRunModuleClosure("sessions"))
	require.ErrorContains(t, err, "ambiguous")
}

// Qualifying the entry is what the refusal asks for, and the flow then builds.
func TestNewFlowAcceptsAQualifiedManagedService(t *testing.T) {
	ctx := t.Context()
	workspace := ambiguousManagedWorkspace(t, "sessions/redis")
	module, err := workspace.LoadModuleFromName(ctx, "sessions")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "redis")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, "staging")
	require.NoError(t, err)

	_, err = NewFlow(ctx, workspace, module, service, env, RunMode)
	require.NoError(t, err)
}
