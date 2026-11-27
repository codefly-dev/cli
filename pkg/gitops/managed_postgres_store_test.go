package gitops

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestManagedPostgresStoreInventory(t *testing.T) {
	var service resources.Service
	require.NoError(t, yaml.Unmarshal([]byte("name: store\nagent: {kind: 'codefly:service', publisher: codefly.dev, name: postgres}\nspec: {database-name: users}"), &service))
	placement := environments.EnvironmentManagedService{Carried: environments.Carried{
		"instance-connection-name": {Node: yaml.Node{Kind: yaml.ScalarNode, Value: "project:region:server"}},
	}}
	store, err := managedPostgresStore(&service, placement)
	require.NoError(t, err)
	require.Equal(t, &InventoryPostgresStore{Database: "users", Instance: "project:region:server"}, store)
	_, err = managedPostgresStore(&service, environments.EnvironmentManagedService{})
	require.ErrorContains(t, err, "instance-connection-name")
	service.Spec = nil
	_, err = managedPostgresStore(&service, placement)
	require.ErrorContains(t, err, "database-name")
	service.Agent.Name = "redis"
	store, err = managedPostgresStore(&service, placement)
	require.NoError(t, err)
	require.Nil(t, store)
}

func TestManagedStoreInventoryIncludesPathlessStoresAndPreservesLegacyAbsence(t *testing.T) {
	inventory := Inventory{Module: "saas"}
	stores := managedStoreDeclarations(&inventory)
	require.Nil(t, stores)
	inventory.StoreInventoryComplete = true
	stores = managedStoreDeclarations(&inventory)
	require.NotNil(t, stores)
	require.Empty(t, *stores)
	inventory.Units = []InventoryUnit{{Kind: UnitKindService, Module: "saas", Name: "store", Managed: true, PostgresStore: &InventoryPostgresStore{Database: "users", Instance: "project:region:server"}}}
	stores = managedStoreDeclarations(&inventory)
	require.Len(t, *stores, 1)
	require.Equal(t, "saas/store", (*stores)[0].Service)
	require.Equal(t, "users", (*stores)[0].Database)
}

func TestStoreDeclarationRequiresACompleteManagedUnit(t *testing.T) {
	unit := InventoryUnit{
		Module: "shared", Name: "journal", Kind: UnitKindService, Managed: true,
		PostgresStore: &InventoryPostgresStore{Database: "events", Instance: "project:region:server"},
	}
	inventory := Inventory{Module: unit.Module, Units: []InventoryUnit{unit}}
	require.ErrorContains(t, validateInventoryUnits(&inventory), "invalid managed Postgres inventory")
	inventory.StoreInventoryComplete = true
	require.NoError(t, validateInventoryUnits(&inventory))
	inventory.Units[0].Managed = false
	require.ErrorContains(t, validateInventoryUnits(&inventory), "invalid managed Postgres inventory")
}
