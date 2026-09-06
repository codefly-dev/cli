package gitops

import (
	"fmt"
	"strings"
	"testing"
)

const testContractDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testContractDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// hostInventory builds the "saas" host module inventory: a single service unit
// "accounts" exposing "connect" (saas.accounts.v1) at the given package version
// and digest.
func hostInventory(version, digest string) Inventory {
	return Inventory{
		SchemaVersion: SchemaVersion,
		Module:        "saas",
		Package:       &InventoryPackage{ID: "codefly/saas-starter", Version: version},
		Units: []InventoryUnit{
			{
				Kind: UnitKindService, Module: "saas", Name: "accounts",
				Contracts: []InventoryContract{
					{Role: ContractRoleExposes, Module: "saas", Service: "accounts", Endpoint: "connect", Package: "saas.accounts.v1", Digest: digest},
				},
			},
		},
	}
}

// consumerInventory builds the "lastlogin" solution inventory consuming
// saas/accounts/connect under the given version/constraint/digest.
func consumerInventory(version, constraint, digest string) Inventory {
	return Inventory{
		SchemaVersion: SchemaVersion,
		Module:        "lastlogin",
		Units: []InventoryUnit{
			{
				Kind: UnitKindSolution, Module: "lastlogin", Name: "lastlogin",
				Contracts: []InventoryContract{
					{
						Role: ContractRoleConsumes, Module: "saas", Service: "accounts", Endpoint: "connect",
						Package: "saas.accounts.v1", Digest: digest, Version: version, Constraint: constraint,
					},
				},
			},
		},
	}
}

func resolverFor(host *Inventory) func(string) (*Inventory, error) {
	return func(module string) (*Inventory, error) {
		if module != "saas" || host == nil {
			return nil, fmt.Errorf("module %s not found", module)
		}
		return host, nil
	}
}

func TestCheckContractsAcceptsExactPinAtMatchingDigest(t *testing.T) {
	host := hostInventory("0.1.0", testContractDigestA)
	consumer := consumerInventory("0.1.0", "", testContractDigestA)

	checks := checkContracts(consumer, resolverFor(&host))
	if len(checks) != 1 || checks[0].Status != ContractCheckOK {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestCheckContractsReportsDriftForCompatibleNewerHostWithDifferentDigest(t *testing.T) {
	host := hostInventory("0.1.3", testContractDigestB)
	consumer := consumerInventory("0.1.0", ">=0.1.0 <0.2.0", testContractDigestA)

	checks := checkContracts(consumer, resolverFor(&host))
	if len(checks) != 1 || checks[0].Status != ContractCheckDrift {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestCheckContractsRejectsHostViolatingConstraint(t *testing.T) {
	host := hostInventory("0.1.3", testContractDigestA)
	consumer := consumerInventory("0.1.0", ">=0.2.0", testContractDigestA)

	checks := checkContracts(consumer, resolverFor(&host))
	if len(checks) != 1 || checks[0].Status != ContractCheckViolation || !strings.Contains(checks[0].Message, "does not satisfy") {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestCheckContractsRejectsUndeployedModule(t *testing.T) {
	consumer := consumerInventory("0.1.0", "", testContractDigestA)

	checks := checkContracts(consumer, resolverFor(nil))
	if len(checks) != 1 || checks[0].Status != ContractCheckViolation || !strings.Contains(checks[0].Message, "is not deployed") {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestCheckContractsRejectsWhenEndpointNoLongerExposed(t *testing.T) {
	host := hostInventory("0.1.0", testContractDigestA)
	host.Units[0].Contracts[0].Endpoint = "disconnect"
	consumer := consumerInventory("0.1.0", "", testContractDigestA)

	checks := checkContracts(consumer, resolverFor(&host))
	if len(checks) != 1 || checks[0].Status != ContractCheckViolation || !strings.Contains(checks[0].Message, "no longer exposes") {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestCheckContractsIgnoresSchemaFourInventoryWithNoContracts(t *testing.T) {
	consumer := Inventory{
		SchemaVersion: priorSchemaVersion,
		Module:        "lastlogin",
		Units: []InventoryUnit{
			{Kind: UnitKindSolution, Module: "lastlogin", Name: "lastlogin"},
		},
	}

	checks := checkContracts(consumer, resolverFor(nil))
	if len(checks) != 0 {
		t.Fatalf("checks = %+v, want none", checks)
	}
}
