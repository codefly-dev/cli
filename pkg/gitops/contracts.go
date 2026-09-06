package gitops

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Masterminds/semver/v3"
	"github.com/codefly-dev/core/composition"
)

// contractCatalogPath is the module-relative location of the catalog of API
// contracts a module exposes, produced from its contracts/api sources.
const contractCatalogPath = "contracts/api/catalog.codefly.json"

type contractCatalogDocument struct {
	Services []contractCatalogService `json:"services"`
}

type contractCatalogService struct {
	Service   string                    `json:"service"`
	Endpoints []contractCatalogEndpoint `json:"endpoints"`
}

type contractCatalogEndpoint struct {
	Endpoint string `json:"endpoint"`
	Package  string `json:"package"`
	Digest   string `json:"digest"`
}

// loadContractCatalog reads a module's contracts/api/catalog.codefly.json, or
// returns a nil document when the module has not exported any API contracts.
func loadContractCatalog(moduleDir string) (*contractCatalogDocument, error) {
	data, err := os.ReadFile(filepath.Join(moduleDir, filepath.FromSlash(contractCatalogPath)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read contract catalog: %w", err)
	}
	var document contractCatalogDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode contract catalog: %w", err)
	}
	return &document, nil
}

// exposedContracts returns the InventoryContract entries a service exposes
// according to the catalog, sorted by endpoint for deterministic rendering.
func (catalog *contractCatalogDocument) exposedContracts(moduleName, serviceName string) []InventoryContract {
	if catalog == nil {
		return nil
	}
	var contracts []InventoryContract
	for _, service := range catalog.Services {
		if service.Service != serviceName {
			continue
		}
		for _, endpoint := range service.Endpoints {
			contracts = append(contracts, InventoryContract{
				Role: ContractRoleExposes, Module: moduleName, Service: serviceName,
				Endpoint: endpoint.Endpoint, Package: endpoint.Package, Digest: endpoint.Digest,
			})
		}
	}
	sort.Slice(contracts, func(i, j int) bool { return contracts[i].Endpoint < contracts[j].Endpoint })
	return contracts
}

// modulePackage loads a module's package manifest (module.package.codefly.yaml)
// and returns its identity, or nil when the module carries no package manifest.
func modulePackage(moduleDir string) (*InventoryPackage, error) {
	manifest, err := composition.LoadPackageManifest(moduleDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load module package manifest: %w", err)
	}
	return &InventoryPackage{ID: manifest.ID, Version: manifest.Version}, nil
}

// checkContracts admits every consumed contract carried by the consumer
// inventory's units against the exposing module's inventory, resolved through
// resolve. It is pure and side-effect free: resolve is the only way it reaches
// outside the consumer inventory, which is what lets tests exercise it against
// hand-built inventories without a real GitOps checkout.
func checkContracts(consumer Inventory, resolve func(module string) (*Inventory, error)) []ContractCheck { //nolint:gocritic // by-value consumer keeps this the pure, hand-buildable admission entry point tests exercise directly
	var checks []ContractCheck
	for _, unit := range consumer.Units {
		for _, contract := range unit.Contracts { //nolint:gocritic // admission runs once per publish over a small unit/contract graph, not a hot path
			if contract.Role != ContractRoleConsumes {
				continue
			}
			checks = append(checks, checkContract(unit.Name, contract, resolve))
		}
	}
	return checks
}

func checkContract(unitName string, contract InventoryContract, resolve func(string) (*Inventory, error)) ContractCheck { //nolint:gocritic // called once per consumed contract in checkContracts' loop, not a hot path
	check := ContractCheck{Unit: unitName, Module: contract.Module, Service: contract.Service, Endpoint: contract.Endpoint}
	host, err := resolve(contract.Module)
	if err != nil || host == nil {
		check.Status = ContractCheckViolation
		check.Message = fmt.Sprintf(
			"consumes %s/%s/%s but module %s is not deployed in the target GitOps tree",
			contract.Module, contract.Service, contract.Endpoint, contract.Module,
		)
		return check
	}
	var exposed *InventoryContract
	for _, hostUnit := range host.Units {
		if hostUnit.Name != contract.Service {
			continue
		}
		for i := range hostUnit.Contracts {
			candidate := hostUnit.Contracts[i]
			if candidate.Role == ContractRoleExposes && candidate.Endpoint == contract.Endpoint {
				exposed = &candidate
				break
			}
		}
	}
	if exposed == nil {
		check.Status = ContractCheckViolation
		check.Message = fmt.Sprintf("module %s no longer exposes %s/%s", contract.Module, contract.Service, contract.Endpoint)
		return check
	}
	if host.Package == nil {
		check.Status = ContractCheckViolation
		check.Message = fmt.Sprintf(
			"module %s has no package version recorded and does not satisfy consumed contract %s/%s",
			contract.Module, contract.Service, contract.Endpoint,
		)
		return check
	}
	compatibleNewerHost := false
	if contract.Constraint != "" {
		constraint, err := semver.NewConstraint(contract.Constraint)
		if err != nil {
			check.Status = ContractCheckViolation
			check.Message = fmt.Sprintf("consumed constraint %q is invalid: %v", contract.Constraint, err)
			return check
		}
		hostVersion, err := semver.NewVersion(host.Package.Version)
		if err != nil {
			check.Status = ContractCheckViolation
			check.Message = fmt.Sprintf("module %s package version %q is invalid", contract.Module, host.Package.Version)
			return check
		}
		if !constraint.Check(hostVersion) {
			check.Status = ContractCheckViolation
			check.Message = fmt.Sprintf(
				"module %s package %s does not satisfy consumed constraint %s",
				contract.Module, host.Package.Version, contract.Constraint,
			)
			return check
		}
		compatibleNewerHost = host.Package.Version != contract.Version
	} else if host.Package.Version != contract.Version {
		check.Status = ContractCheckViolation
		check.Message = fmt.Sprintf(
			"module %s package %s does not satisfy pinned version %s",
			contract.Module, host.Package.Version, contract.Version,
		)
		return check
	}
	if exposed.Digest != contract.Digest {
		if compatibleNewerHost {
			check.Status = ContractCheckDrift
			check.Message = fmt.Sprintf(
				"module %s package %s satisfies %s but the client was generated from an older contract",
				contract.Module, host.Package.Version, contract.Constraint,
			)
			return check
		}
		check.Status = ContractCheckViolation
		check.Message = fmt.Sprintf(
			"module %s contract digest %s does not match consumed digest %s",
			contract.Module, exposed.Digest, contract.Digest,
		)
		return check
	}
	check.Status = ContractCheckOK
	return check
}
