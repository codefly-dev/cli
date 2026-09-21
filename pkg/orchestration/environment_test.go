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
      fixture: dev-admin
      namespace: apps
      cluster:
          kind: k3d
          kubeconfig: ~/.kube/k3d.yaml
          context: k3d-dev
      registry:
          url: localhost:5001
          auth: ecr
      service-secrets:
          secret-store:
              name: default-secrets
              kind: ClusterSecretStore
          services:
              api:
                  secret-store:
                      name: api-secrets
                      kind: SecretStore
                  remote-keys:
                      TOKEN: api/token
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
	require.Equal(t, "dev-admin", env.Fixture)
	require.Equal(t, "apps", env.Namespace)
	require.NotNil(t, env.Cluster)
	require.Equal(t, "k3d", env.Cluster.Kind)
	require.Equal(t, "~/.kube/k3d.yaml", env.Cluster.Kubeconfig)
	require.Equal(t, "k3d-dev", env.Cluster.Context)
	require.NotNil(t, env.Registry)
	require.Equal(t, "localhost:5001", env.Registry.URL)
	require.Equal(t, "ecr", env.Registry.Auth)
	require.Equal(t, "default-secrets", env.ServiceSecrets.SecretStore.Name)
	require.Equal(t, "api-secrets", env.ServiceSecrets.Services["api"].SecretStore.Name)
	require.Equal(t, "api/token", env.ServiceSecrets.Services["api"].RemoteKeys["TOKEN"].Key)
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
	env.ServiceSecrets.SecretStore.Name = "other-default"
	apiSecrets := env.ServiceSecrets.Services["api"]
	apiSecrets.SecretStore.Name = "other-api"
	apiSecrets.RemoteKeys["TOKEN"] = resources.EnvironmentSecretRemoteRef{Key: "other/token"}
	env.ServiceSecrets.Services["api"] = apiSecrets
	env.Secrets[0].Account = "other-account"

	declared := workspace.FindEnvironment(LocalEnvironmentName)
	require.Equal(t, "from-yaml", declared.NamingScope)
	require.Equal(t, "apps", declared.Namespace)
	require.Equal(t, "~/.kube/k3d.yaml", declared.Cluster.Kubeconfig)
	require.Equal(t, "localhost:5001", declared.Registry.URL)
	require.Equal(t, "default-secrets", declared.ServiceSecrets.SecretStore.Name)
	require.Equal(t, "api-secrets", declared.ServiceSecrets.Services["api"].SecretStore.Name)
	require.Equal(t, "api/token", declared.ServiceSecrets.Services["api"].RemoteKeys["TOKEN"].Key)
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

// cloneEnvironment is correct by enumeration, not by construction: every place a
// resources.Environment holds a pointer, map or slice is a place the clone must
// copy rather than share. This canary walks the whole type graph, so a field
// added to a nested type trips it too — keyed on top-level names alone it passed
// while EnvironmentServiceConfigMapping went entirely unexamined.
func TestCloneEnvironmentCoversEveryEnvironmentField(t *testing.T) {
	deepCopied := map[string]bool{
		".Cluster":   true,
		".Registry":  true,
		".Gitops":    true,
		".Dns":       true,
		".Secrets":   true,
		".Secrets[]": true,

		".Ingress":         true,
		".Ingress[].Hosts": true,

		".ManagedServices":                        true,
		".ManagedServices[].EgressCIDRs":          true,
		".ManagedServices[].SecretReferences":     true,
		".ManagedServices[].Identity":             true,
		".ManagedServices[].Identity.Annotations": true,
		".ManagedServices[].Identity.Labels":      true,

		".ServiceSecrets":                        true,
		".ServiceSecrets.Services":               true,
		".ServiceSecrets.Services[].SecretStore": true,
		".ServiceSecrets.Services[].RemoteKeys":  true,
		".ServiceSecrets.Services[].Defaults":    true,

		".ServiceConfig":                   true,
		".ServiceConfig.Services":          true,
		".ServiceConfig.Services[].Values": true,

		".ResourceQuota":                           true,
		".ResourceQuota.Requests":                  true,
		".ResourceQuota.Limits":                    true,
		".ResourceQuota.DefaultContainer":          true,
		".ResourceQuota.DefaultContainer.Requests": true,
		".ResourceQuota.DefaultContainer.Limits":   true,
	}

	found := referencePaths(reflect.TypeOf(resources.Environment{}))
	for _, path := range found {
		if !deepCopied[path] {
			t.Errorf("resources.Environment%s is shared, not copied, by cloneEnvironment — extend the clone before concurrent flows can contaminate each other", path)
		}
	}

	// A path core has dropped must not linger here, or the list stops describing
	// the clone and the next real gap hides behind a stale entry.
	present := make(map[string]bool, len(found))
	for _, path := range found {
		present[path] = true
	}
	for path := range deepCopied {
		if !present[path] {
			t.Errorf("deepCopied lists resources.Environment%s, which no longer exists — drop it", path)
		}
	}
}

// referencePaths returns every path within t that reaches a pointer, map or
// slice: the locations a struct assignment copies by reference. A pointer is
// transparent in the path, a map or slice element is spelled "[]".
func referencePaths(t reflect.Type) []string {
	var paths []string
	onPath := map[reflect.Type]bool{}

	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, prefix string) {
		switch typ.Kind() {
		case reflect.Ptr:
			paths = append(paths, prefix)
			descend(typ.Elem(), prefix, onPath, walk)
		case reflect.Map, reflect.Slice:
			paths = append(paths, prefix)
			descend(typ.Elem(), prefix+"[]", onPath, walk)
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				walk(field.Type, prefix+"."+field.Name)
			}
		}
	}
	walk(t, "")
	return paths
}

// descend recurses into an element type, refusing to re-enter a type already on
// the current path so a self-referential contract cannot spin forever.
func descend(elem reflect.Type, prefix string, onPath map[reflect.Type]bool, walk func(reflect.Type, string)) {
	if onPath[elem] {
		return
	}
	onPath[elem] = true
	defer delete(onPath, elem)
	walk(elem, prefix)
}

func TestCloneEnvironmentIsolatesServiceConfig(t *testing.T) {
	original := &resources.Environment{
		Name: "azure",
		ServiceConfig: &resources.EnvironmentServiceConfig{
			Services: map[string]resources.EnvironmentServiceConfigMapping{
				"frontend": {Values: map[string]string{"region": "eastus2"}},
			},
		},
	}
	clone := cloneEnvironment(original)

	require.NotSame(t, original.ServiceConfig, clone.ServiceConfig)
	require.Equal(t, "eastus2", clone.ServiceConfig.Services["frontend"].Values["region"])

	// Mutating the clone must not contaminate the original a concurrent flow holds.
	clone.ServiceConfig.Services["frontend"].Values["region"] = "westus"
	clone.ServiceConfig.Services["api"] = resources.EnvironmentServiceConfigMapping{}
	require.Equal(t, "eastus2", original.ServiceConfig.Services["frontend"].Values["region"])
	require.NotContains(t, original.ServiceConfig.Services, "api")
}

// An explicitly empty services map is still a shared header until it is copied.
func TestCloneEnvironmentIsolatesAnEmptyServiceConfig(t *testing.T) {
	original := &resources.Environment{
		Name:          "azure",
		ServiceConfig: &resources.EnvironmentServiceConfig{Services: map[string]resources.EnvironmentServiceConfigMapping{}},
	}
	clone := cloneEnvironment(original)

	clone.ServiceConfig.Services["api"] = resources.EnvironmentServiceConfigMapping{}
	require.NotContains(t, original.ServiceConfig.Services, "api")
}

// An empty map decoded from a workspace ("managed-services: {}") is non-nil, so
// the clone shares its header until it is copied.
func TestCloneEnvironmentIsolatesEmptyMaps(t *testing.T) {
	original := &resources.Environment{
		Name:            "azure",
		ManagedServices: map[string]resources.EnvironmentManagedService{},
		ServiceSecrets: &resources.EnvironmentServiceSecrets{
			Services: map[string]resources.EnvironmentServiceSecretMapping{},
		},
	}
	clone := cloneEnvironment(original)

	clone.ManagedServices["store"] = resources.EnvironmentManagedService{}
	clone.ServiceSecrets.Services["api"] = resources.EnvironmentServiceSecretMapping{}

	require.NotContains(t, original.ManagedServices, "store")
	require.NotContains(t, original.ServiceSecrets.Services, "api")
}

// RemoteKeys hangs off a per-service mapping copied by value, so its map aliases
// the original's until it too is copied.
func TestCloneEnvironmentIsolatesEmptyRemoteKeys(t *testing.T) {
	original := &resources.Environment{
		Name: "azure",
		ServiceSecrets: &resources.EnvironmentServiceSecrets{
			Services: map[string]resources.EnvironmentServiceSecretMapping{
				"api": {RemoteKeys: map[string]resources.EnvironmentSecretRemoteRef{}},
			},
		},
	}
	clone := cloneEnvironment(original)

	clone.ServiceSecrets.Services["api"].RemoteKeys["token"] = resources.EnvironmentSecretRemoteRef{}

	require.NotContains(t, original.ServiceSecrets.Services["api"].RemoteKeys, "token")
}

// A nil services map must stay nil, so the environment re-serializes without an
// empty service-config block it never declared.
func TestCloneEnvironmentKeepsANilServiceConfigMapNil(t *testing.T) {
	original := &resources.Environment{
		Name:          "azure",
		ServiceConfig: &resources.EnvironmentServiceConfig{},
	}
	clone := cloneEnvironment(original)

	require.Nil(t, clone.ServiceConfig.Services)
}

func TestCloneEnvironmentIsolatesResourceQuota(t *testing.T) {
	original := &resources.Environment{
		Name: "staging",
		ResourceQuota: &resources.EnvironmentResourceQuota{
			Requests: &resources.EnvironmentResourceList{CPU: "4", Memory: "8Gi"},
			Limits:   &resources.EnvironmentResourceList{CPU: "8", Memory: "16Gi"},
			Pods:     "50",
			DefaultContainer: &resources.EnvironmentContainerResources{
				Requests: &resources.EnvironmentResourceList{CPU: "100m", Memory: "128Mi"},
				Limits:   &resources.EnvironmentResourceList{CPU: "500m", Memory: "512Mi"},
			},
		},
	}
	clone := cloneEnvironment(original)

	require.NotSame(t, original.ResourceQuota, clone.ResourceQuota)
	require.NotSame(t, original.ResourceQuota.Requests, clone.ResourceQuota.Requests)
	require.NotSame(t, original.ResourceQuota.Limits, clone.ResourceQuota.Limits)
	require.NotSame(t, original.ResourceQuota.DefaultContainer, clone.ResourceQuota.DefaultContainer)
	require.NotSame(t, original.ResourceQuota.DefaultContainer.Requests, clone.ResourceQuota.DefaultContainer.Requests)
	require.NotSame(t, original.ResourceQuota.DefaultContainer.Limits, clone.ResourceQuota.DefaultContainer.Limits)

	// Mutating the clone must not contaminate the original a concurrent flow holds.
	clone.ResourceQuota.Requests.CPU = "99"
	clone.ResourceQuota.DefaultContainer.Limits.Memory = "1Gi"
	require.Equal(t, "4", original.ResourceQuota.Requests.CPU)
	require.Equal(t, "512Mi", original.ResourceQuota.DefaultContainer.Limits.Memory)
}

func TestCloneEnvironmentIsolatesDns(t *testing.T) {
	original := &resources.Environment{
		Name: "azure",
		Dns:  &resources.EnvironmentDNS{AppHostSuffix: "staging.eastus2.azure.example.com"},
	}
	clone := cloneEnvironment(original)

	require.NotSame(t, original.Dns, clone.Dns)
	require.Equal(t, original.Dns.AppHostSuffix, clone.Dns.AppHostSuffix)

	// Mutating the clone must not contaminate the original a concurrent flow holds.
	clone.Dns.AppHostSuffix = "other.example.com"
	require.Equal(t, "staging.eastus2.azure.example.com", original.Dns.AppHostSuffix)
}

func TestCloneEnvironmentIsolatesManagedServiceIdentity(t *testing.T) {
	original := &resources.Environment{ManagedServices: map[string]resources.EnvironmentManagedService{
		"endpoint": {Identity: &resources.EnvironmentWorkloadIdentity{
			Principal: "owner", Annotations: map[string]string{"identity": "owner"}, Labels: map[string]string{"enabled": "true"},
		}},
	}}
	clone := cloneEnvironment(original)
	identity := clone.ManagedServices["endpoint"].Identity
	identity.Principal = "another"
	identity.Annotations["identity"] = "another"
	identity.Labels["enabled"] = "false"
	want := original.ManagedServices["endpoint"].Identity
	require.Equal(t, "owner", want.Principal)
	require.Equal(t, "owner", want.Annotations["identity"])
	require.Equal(t, "true", want.Labels["enabled"])
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

func TestSelectedFixturePrefersOverrideThenEnvironment(t *testing.T) {
	declared := &resources.Environment{Fixture: "dev-admin"}

	// The common case: an environment declares its fixture, so neither
	// `codefly run service` nor `codefly test service` needs --fixture.
	require.Equal(t, "dev-admin", SelectedFixture(declared, ""))

	// An explicit override always wins.
	require.Equal(t, "custom", SelectedFixture(declared, "custom"))

	// An environment that deliberately omits a fixture keeps loading its real
	// provider configuration instead of silently falling back to one.
	require.Empty(t, SelectedFixture(&resources.Environment{}, ""))
	require.Equal(t, "custom", SelectedFixture(&resources.Environment{}, "custom"))
	require.Empty(t, SelectedFixture(nil, ""))
}
