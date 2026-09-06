package gitops

import (
	"encoding/json"
	"fmt"
	"os"
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

// resolverFor returns a resolve function that finds "saas" only when host is
// non-nil, and reports a genuinely absent module the same way the real
// resolveGitopsModuleInventory does (an error satisfying
// errors.Is(err, os.ErrNotExist)) so tests exercise the same not-deployed vs.
// broken-inventory distinction checkContract makes in production.
func resolverFor(host *Inventory) func(string) (*Inventory, error) {
	return func(module string) (*Inventory, error) {
		if module != "saas" || host == nil {
			return nil, fmt.Errorf("module %s: %w", module, os.ErrNotExist)
		}
		return host, nil
	}
}

func TestCheckContractsAcceptsExactPinAtMatchingDigest(t *testing.T) {
	host := hostInventory("0.1.0", testContractDigestA)
	consumer := consumerInventory("0.1.0", "", testContractDigestA)

	checks := checkContracts(consumer, resolverFor(&host), false)
	if len(checks) != 1 || checks[0].Status != ContractCheckOK {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestCheckContractsReportsDriftForCompatibleNewerHostWithDifferentDigest(t *testing.T) {
	host := hostInventory("0.1.3", testContractDigestB)
	consumer := consumerInventory("0.1.0", ">=0.1.0 <0.2.0", testContractDigestA)

	checks := checkContracts(consumer, resolverFor(&host), false)
	if len(checks) != 1 || checks[0].Status != ContractCheckDrift {
		t.Fatalf("checks = %+v", checks)
	}
}

// TestCheckContractsRejectsOlderHostEvenWhenConstraintSatisfied is the
// regression test for the drift/violation inversion bug: a host that
// satisfies the consumer's constraint but is actually OLDER than the version
// the client was generated from (e.g. the host was rolled back) must not be
// classified as "drift" just because its version string differs from the
// pinned one. Only a host that is semantically newer is a benign upgrade.
func TestCheckContractsRejectsOlderHostEvenWhenConstraintSatisfied(t *testing.T) {
	host := hostInventory("1.0.0", testContractDigestB)
	consumer := consumerInventory("1.5.0", ">=0.1.0 <2.0.0", testContractDigestA)

	checks := checkContracts(consumer, resolverFor(&host), false)
	if len(checks) != 1 || checks[0].Status != ContractCheckViolation || !strings.Contains(checks[0].Message, "does not match") {
		t.Fatalf("checks = %+v, want a violation for an older host with a different digest", checks)
	}
}

func TestCheckContractsRejectsHostViolatingConstraint(t *testing.T) {
	host := hostInventory("0.1.3", testContractDigestA)
	consumer := consumerInventory("0.1.0", ">=0.2.0", testContractDigestA)

	checks := checkContracts(consumer, resolverFor(&host), false)
	if len(checks) != 1 || checks[0].Status != ContractCheckViolation || !strings.Contains(checks[0].Message, "does not satisfy") {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestCheckContractsRejectsUndeployedModule(t *testing.T) {
	consumer := consumerInventory("0.1.0", "", testContractDigestA)

	checks := checkContracts(consumer, resolverFor(nil), false)
	if len(checks) != 1 || checks[0].Status != ContractCheckViolation || !strings.Contains(checks[0].Message, "is not deployed") {
		t.Fatalf("checks = %+v", checks)
	}
}

// TestCheckContractsSkipsUndeployedModuleWhenAllowed exercises
// --allow-unresolved-contracts: a genuinely absent host module (resolve fails
// with os.ErrNotExist) is downgraded to skipped.
func TestCheckContractsSkipsUndeployedModuleWhenAllowed(t *testing.T) {
	consumer := consumerInventory("0.1.0", "", testContractDigestA)

	checks := checkContracts(consumer, resolverFor(nil), true)
	if len(checks) != 1 || checks[0].Status != ContractCheckSkipped {
		t.Fatalf("checks = %+v, want skipped", checks)
	}
}

// TestCheckContractsNeverDowngradesACorruptHostInventoryEvenWhenAllowed is the
// regression test for the error-conflation bug: a resolve failure that is NOT
// "the module has no inventory yet" (a decode error, a non-canonical file, an
// unsupported schema, a path-safety rejection) must stay a hard violation and
// must carry the real error text, regardless of --allow-unresolved-contracts.
func TestCheckContractsNeverDowngradesACorruptHostInventoryEvenWhenAllowed(t *testing.T) {
	consumer := consumerInventory("0.1.0", "", testContractDigestA)
	resolve := func(module string) (*Inventory, error) {
		return nil, fmt.Errorf("%s inventory is not canonical", module)
	}

	for _, allow := range []bool{false, true} {
		checks := checkContracts(consumer, resolve, allow)
		if len(checks) != 1 || checks[0].Status != ContractCheckViolation {
			t.Fatalf("allow=%v: checks = %+v, want a violation regardless of the flag", allow, checks)
		}
		if !strings.Contains(checks[0].Message, "is not canonical") {
			t.Fatalf("allow=%v: message = %q, want the real resolve error preserved", allow, checks[0].Message)
		}
		if strings.Contains(checks[0].Message, "is not deployed") {
			t.Fatalf("allow=%v: message = %q, a corrupt inventory must not read as \"not deployed\"", allow, checks[0].Message)
		}
	}
}

func TestCheckContractsRejectsWhenEndpointNoLongerExposed(t *testing.T) {
	host := hostInventory("0.1.0", testContractDigestA)
	host.Units[0].Contracts[0].Endpoint = "disconnect"
	consumer := consumerInventory("0.1.0", "", testContractDigestA)

	checks := checkContracts(consumer, resolverFor(&host), false)
	if len(checks) != 1 || checks[0].Status != ContractCheckViolation || !strings.Contains(checks[0].Message, "no longer exposes") {
		t.Fatalf("checks = %+v", checks)
	}
}

// TestCheckContractsRejectsWhenExposedProtoPackageChanged proves the captured
// contract.Package / exposed.Package fields are actually compared: an
// endpoint name that is reused for a different proto package must not be
// treated as satisfying the consumed contract, even if the digest happened to
// coincide.
func TestCheckContractsRejectsWhenExposedProtoPackageChanged(t *testing.T) {
	host := hostInventory("0.1.0", testContractDigestA)
	host.Units[0].Contracts[0].Package = "saas.notifications.v1"
	consumer := consumerInventory("0.1.0", "", testContractDigestA)

	checks := checkContracts(consumer, resolverFor(&host), false)
	if len(checks) != 1 || checks[0].Status != ContractCheckViolation || !strings.Contains(checks[0].Message, "not the consumed package") {
		t.Fatalf("checks = %+v", checks)
	}
}

// TestCheckContractsHandlesPriorSchemaInventoryWithNoContracts decodes a
// realistic pre-migration render (schemaVersion 4, no "contracts" key
// anywhere) from raw JSON rather than hand-setting the field, so it actually
// exercises the migration path rather than the trivial "empty slice" case.
func TestCheckContractsHandlesPriorSchemaInventoryWithNoContracts(t *testing.T) {
	raw := `{
		"schemaVersion": 4,
		"module": "lastlogin",
		"environment": "production",
		"appProject": "lastlogin",
		"ownedPath": "deployments/modules/lastlogin",
		"units": [
			{"kind": "solution", "module": "lastlogin", "name": "lastlogin", "path": "solutions/lastlogin"}
		],
		"files": [],
		"digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	}`
	var consumer Inventory
	if err := json.Unmarshal([]byte(raw), &consumer); err != nil {
		t.Fatal(err)
	}
	if consumer.Package != nil || consumer.Units[0].Contracts != nil {
		t.Fatalf("schema-4 JSON unexpectedly decoded provenance fields: %+v", consumer)
	}

	checks := checkContracts(consumer, resolverFor(nil), false)
	if len(checks) != 0 {
		t.Fatalf("checks = %+v, want none", checks)
	}
}
