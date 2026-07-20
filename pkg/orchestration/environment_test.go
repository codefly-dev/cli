package orchestration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const declaredEnvironmentsWorkspace = `name: env-select
layout: flat
environments:
    - name: local
      description: developer machine
      naming-scope: from-yaml
      namespace: apps
      cluster:
          kind: k3d
          kubeconfig: ~/.kube/k3d.yaml
          context: k3d-dev
      registry:
          url: localhost:5001
          auth: ecr
      secrets:
          - kind: 1password
            account: acme-dev
    - name: staging
      description: shared staging
      naming-scope: stg
      namespace: staging-apps
      cluster:
          kind: eks
          kubeconfig: ~/.kube/staging.yaml
      registry:
          url: 123456789.dkr.ecr.us-east-1.amazonaws.com/acme
          auth: ecr
      secrets:
          - kind: 1password
            account: acme-staging
`

func writeTempWorkspace(t *testing.T, files map[string]string) *resources.Workspace {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), dir)
	require.NoError(t, err)
	return workspace
}

func declaredWorkspace(t *testing.T) *resources.Workspace {
	t.Helper()
	return writeTempWorkspace(t, map[string]string{"workspace.codefly.yaml": declaredEnvironmentsWorkspace})
}

func TestSelectEnvironmentDeclaredLocalKeepsEveryField(t *testing.T) {
	workspace := declaredWorkspace(t)

	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	require.Equal(t, "local", env.Name)
	require.Equal(t, "developer machine", env.Description)
	require.Equal(t, "from-yaml", env.NamingScope)
	require.Equal(t, "apps", env.Namespace)
	require.NotNil(t, env.Cluster)
	require.Equal(t, "k3d", env.Cluster.Kind)
	require.Equal(t, "~/.kube/k3d.yaml", env.Cluster.Kubeconfig)
	require.Equal(t, "k3d-dev", env.Cluster.Context)
	require.NotNil(t, env.Registry)
	require.Equal(t, "localhost:5001", env.Registry.URL)
	require.Equal(t, "ecr", env.Registry.Auth)
	require.Len(t, env.Secrets, 1)
	require.Equal(t, "1password", env.Secrets[0].Kind)
	require.Equal(t, "acme-dev", env.Secrets[0].Account)
}

func TestSelectEnvironmentUndeclaredLocalKeepsLegacyDefault(t *testing.T) {
	workspace := writeTempWorkspace(t, map[string]string{"workspace.codefly.yaml": "name: bare\nlayout: flat\n"})

	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	require.Equal(t, resources.LocalEnvironment(), env)
}

func TestSelectEnvironmentDeclaredNonLocalIsSelectedExactly(t *testing.T) {
	workspace := declaredWorkspace(t)

	env, err := SelectEnvironment(workspace, "staging")
	require.NoError(t, err)
	require.Equal(t, workspace.FindEnvironment("staging"), env)
	require.NotSame(t, workspace.FindEnvironment("staging"), env)
}

func TestSelectEnvironmentMissingNonLocalFailsWithoutSecretMaterial(t *testing.T) {
	workspace := declaredWorkspace(t)

	env, err := SelectEnvironment(workspace, "production")
	require.Nil(t, env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "env-select")
	require.Contains(t, err.Error(), `"production"`)
	require.NotContains(t, err.Error(), "acme-dev")
	require.NotContains(t, err.Error(), "1password")
}

func TestSelectEnvironmentOverridesDoNotMutateWorkspace(t *testing.T) {
	workspace := declaredWorkspace(t)

	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	env.NamingScope = "override"
	env.Namespace = "elsewhere"
	env.Cluster.Kubeconfig = "/tmp/other"
	env.Registry.URL = "example.com/other"
	env.Secrets[0].Account = "other-account"

	declared := workspace.FindEnvironment(LocalEnvironmentName)
	require.Equal(t, "from-yaml", declared.NamingScope)
	require.Equal(t, "apps", declared.Namespace)
	require.Equal(t, "~/.kube/k3d.yaml", declared.Cluster.Kubeconfig)
	require.Equal(t, "localhost:5001", declared.Registry.URL)
	require.Equal(t, "acme-dev", declared.Secrets[0].Account)

	fresh, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	require.Equal(t, "from-yaml", fresh.NamingScope)
	require.Equal(t, "other-account", env.Secrets[0].Account)
}

func TestSelectEnvironmentConcurrentOverridesDoNotContaminate(t *testing.T) {
	workspace := declaredWorkspace(t)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env, err := SelectEnvironment(workspace, LocalEnvironmentName)
			if err != nil {
				t.Error(err)
				return
			}
			scope := fmt.Sprintf("scope-%d", i)
			env.NamingScope = scope
			env.Secrets[0].Account = fmt.Sprintf("account-%d", i)
			if env.NamingScope != scope {
				t.Errorf("override lost for %d", i)
			}
		}(i)
	}
	wg.Wait()

	declared := workspace.FindEnvironment(LocalEnvironmentName)
	require.Equal(t, "from-yaml", declared.NamingScope)
	require.Equal(t, "acme-dev", declared.Secrets[0].Account)
}

// cloneEnvironment is correct by enumeration, not by construction: it must
// name every non-value field of resources.Environment. This canary fails the
// moment core grows the struct with a field the clone would silently share,
// which would quietly reintroduce cross-flow contamination.
func TestCloneEnvironmentCoversEveryEnvironmentField(t *testing.T) {
	deepCopied := map[string]bool{"Cluster": true, "Registry": true, "Secrets": true}
	typ := reflect.TypeOf(resources.Environment{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if deepCopied[field.Name] {
			continue
		}
		switch field.Type.Kind() {
		case reflect.Bool, reflect.String,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
			// Value kinds are copied by the struct assignment.
		default:
			t.Errorf("resources.Environment.%s (%s) is shared, not copied, by cloneEnvironment — extend the clone before concurrent flows can contaminate each other", field.Name, field.Type)
		}
	}
}

func TestSelectEnvironmentIsEquivalentAcrossFlows(t *testing.T) {
	workspace := declaredWorkspace(t)

	first, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	second, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	require.Equal(t, first, second)
	require.NotSame(t, first, second)
	require.NotSame(t, first.Secrets[0], second.Secrets[0])
}

func TestNewFlowCarriesSelectedEnvironmentEverywhere(t *testing.T) {
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": `name: env-flow
layout: modules
modules:
    - name: web
environments:
    - name: local
      naming-scope: from-yaml
      secrets:
          - kind: 1password
            account: acme-dev
`,
		"modules/web/module.codefly.yaml": `kind: module
name: web
project: env-flow
services:
    - name: gateway
`,
		"modules/web/services/gateway/service.codefly.yaml": `kind: service
name: gateway
version: 0.0.0
module: web
agent:
    kind: runtime::service
    name: krakend
    version: 0.0.6
    publisher: codefly.ai
endpoints:
    - name: rest
      visibility: public
      api: rest
`,
	})

	ctx := context.Background()
	module, err := workspace.LoadModuleFromName(ctx, "web")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "gateway")
	require.NoError(t, err)

	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	flow, err := NewFlow(ctx, workspace, module, service, env, RunMode)
	require.NoError(t, err)

	// The flow's world is the single environment source for the
	// configuration manager, network managers, and every runner's
	// serialized LoadRequest — so honoring the declaration here is
	// honoring it everywhere.
	require.Same(t, env, flow.world.Env)
	require.Same(t, flow.ConfigurationManager, flow.world.ConfigurationManager)
	require.Len(t, flow.world.Env.Secrets, 1)
	require.Equal(t, "acme-dev", flow.world.Env.Secrets[0].Account)

	proto, err := flow.world.Env.Proto()
	require.NoError(t, err)
	require.Equal(t, "local", proto.Name)
	require.Equal(t, "from-yaml", proto.NamingScope)
}
