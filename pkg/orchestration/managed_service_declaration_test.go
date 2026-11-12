package orchestration

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/architecture"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

func yamlScalar(value string) resources.YAMLValue {
	return resources.YAMLValue{Node: yaml.Node{Kind: yaml.ScalarNode, Value: value}}
}

// storeWorkspace composes one module whose store is depended on by a service at
// run time, by one only at schema (build) time, and not at all by a third.
func storeWorkspace(t *testing.T) *World {
	t.Helper()
	files := map[string]string{
		"workspace.codefly.yaml":              "name: platform\nlayout: modules\nmodules:\n  - name: records\n",
		"modules/records/module.codefly.yaml": "name: records\nservices:\n  - name: store\n  - name: records\n  - name: codegen\n  - name: bystander\n",
	}
	edges := map[string]string{
		"records": "service-dependencies:\n  - name: store\n    kind: runtime\n",
		"codegen": "service-dependencies:\n  - name: store\n    kind: schema\n",
	}
	for _, name := range []string{"store", "records", "codegen", "bystander"} {
		files["modules/records/services/"+name+"/service.codefly.yaml"] = "name: " + name + "\nversion: 0.0.0\nagent:\n  kind: codefly:service\n  publisher: codefly.dev\n  name: missing-peer\n  version: 0.0.1\n" + edges[name]
	}
	workspace := writeTempWorkspace(t, files)
	dependencies, err := architecture.NewServiceDependencies(t.Context(), workspace)
	require.NoError(t, err)
	return &World{Workspace: workspace, Dependencies: dependencies}
}

func managedStoreEnvironment() *environments.Environment {
	return &environments.Environment{
		Name: "blue",
		ManagedServices: map[string]environments.EnvironmentManagedService{
			"records/store": {Kind: "postgres", ExternalName: "10.87.0.7", Port: 5432, Carried: environments.Carried{
				"auth-mode":                yamlScalar("external-identity"),
				"instance-connection-name": yamlScalar("obin-blue-data:us-east1:obin-blue-shared"),
			}},
		},
		ServiceIdentity: &environments.EnvironmentServiceIdentity{Services: map[string]environments.EnvironmentWorkloadIdentity{
			"records/store":   {Principal: "obin-blue-db-store@obin-blue-data.iam.gserviceaccount.com"},
			"records/records": {Principal: "obin-blue-b641b7@obin-blue-data.iam.gserviceaccount.com"},
		}},
	}
}

func declarationValues(t *testing.T, configuration *basev0.Configuration, name string) map[string]string {
	t.Helper()
	var found *basev0.ConfigurationInformation
	for _, information := range configuration.GetInfos() {
		if information.GetName() == name {
			require.Nil(t, found, "%s declared twice", name)
			found = information
		}
	}
	require.NotNil(t, found, "no %s information", name)
	values := map[string]string{}
	for _, value := range found.GetConfigurationValues() {
		require.False(t, value.GetSecret(), "%s is not a secret", value.GetKey())
		values[value.GetKey()] = value.GetValue()
	}
	return values
}

// A managed store's agent is told the entry's values, under the names it reads
// them by, who its bootstrap Job logs in as, and who reaches the instance at
// run time — and only at run time: a schema-time dependent generates code, it
// never opens the connection, and a service with no edge is nobody's principal.
func TestManagedStoreAgentIsHandedTheEnvironmentsDeclaration(t *testing.T) {
	ctx := t.Context()
	world := storeWorkspace(t)
	world.Env = managedStoreEnvironment()
	store := &resources.ServiceIdentity{Module: "records", Name: "store"}
	own := &basev0.Configuration{Origin: "records/store", Infos: []*basev0.ConfigurationInformation{{Name: "postgres"}}}
	original := proto.CloneOf(own)

	declared, err := withEnvironmentServiceDeclaration(ctx, own, store, world)
	require.NoError(t, err)
	require.True(t, proto.Equal(original, own), "the service's own configuration was widened")
	require.Len(t, declared.GetInfos(), 2)
	require.Equal(t, map[string]string{
		"login-mode":         "external-identity",
		"instance":           "obin-blue-data:us-east1:obin-blue-shared",
		"owner-principal":    "obin-blue-db-store@obin-blue-data.iam.gserviceaccount.com",
		"runtime-principals": "obin-blue-b641b7@obin-blue-data.iam.gserviceaccount.com",
	}, declarationValues(t, declared, ManagedServiceInformation))
	for _, value := range declared.GetInfos()[1].GetConfigurationValues() {
		require.False(t, resources.IsSensitiveKey(value.GetKey()), "%s reads as a credential and would be refused by a restricted render", value.GetKey())
	}

	// A consumer with no identity of its own takes the one the managed service
	// declares for its consumers, the way the render's pods do.
	world.Env.ServiceIdentity.Services = map[string]environments.EnvironmentWorkloadIdentity{"records/store": {Principal: "owner@obin-blue-data.iam.gserviceaccount.com"}}
	consumers := world.Env.ManagedServices["records/store"]
	consumers.Identity = &environments.EnvironmentWorkloadIdentity{Principal: "consumers@obin-blue-data.iam.gserviceaccount.com"}
	world.Env.ManagedServices["records/store"] = consumers
	declared, err = withEnvironmentServiceDeclaration(ctx, nil, store, world)
	require.NoError(t, err)
	require.Equal(t, "consumers@obin-blue-data.iam.gserviceaccount.com", declarationValues(t, declared, ManagedServiceInformation)["runtime-principals"])

	// A service nothing declares gets its configuration back as it was.
	bystander := &resources.ServiceIdentity{Module: "records", Name: "bystander"}
	unchanged, err := withEnvironmentServiceDeclaration(ctx, own, bystander, world)
	require.NoError(t, err)
	require.Same(t, own, unchanged)
}

// The in-cluster statement is the fact alone, so a store's agent can tell "the
// environment said in-cluster" from "the environment said nothing".
func TestInClusterStoreAgentIsToldSo(t *testing.T) {
	world := storeWorkspace(t)
	world.Env = &environments.Environment{Name: "dev", InClusterServices: []string{"records/store"}}
	store := &resources.ServiceIdentity{Module: "records", Name: "store"}
	declared, err := withEnvironmentServiceDeclaration(t.Context(), nil, store, world)
	require.NoError(t, err)
	require.Equal(t, "records/store", declared.GetOrigin())
	require.Empty(t, declarationValues(t, declared, InClusterServiceInformation))
}

// A managed store whose Job has no identity, or whose runtime consumer has
// none, is refused here by name — not discovered as a Job that cannot log in.
func TestManagedStoreWithoutAnIdentityIsRefusedByName(t *testing.T) {
	ctx := t.Context()
	store := &resources.ServiceIdentity{Module: "records", Name: "store"}

	world := storeWorkspace(t)
	world.Env = managedStoreEnvironment()
	delete(world.Env.ServiceIdentity.Services, "records/store")
	_, err := withEnvironmentServiceDeclaration(ctx, nil, store, world)
	require.ErrorContains(t, err, "declares no service-identity for it")
	require.ErrorContains(t, err, "records/store")

	world = storeWorkspace(t)
	world.Env = managedStoreEnvironment()
	delete(world.Env.ServiceIdentity.Services, "records/records")
	_, err = withEnvironmentServiceDeclaration(ctx, nil, store, world)
	require.ErrorContains(t, err, "records/records depends on managed service records/store at run time but declares no identity")

	// A password-mode managed service names no principal: the identities are
	// the cloud's business only when the workload authenticates as itself.
	world = storeWorkspace(t)
	world.Env = managedStoreEnvironment()
	entry := world.Env.ManagedServices["records/store"]
	entry.Carried = environments.Carried{"auth-mode": yamlScalar("password")}
	world.Env.ManagedServices["records/store"] = entry
	world.Env.ServiceIdentity = nil
	declared, err := withEnvironmentServiceDeclaration(ctx, nil, store, world)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"login-mode": "password"}, declarationValues(t, declared, ManagedServiceInformation))
}
