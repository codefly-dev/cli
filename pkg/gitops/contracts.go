package gitops

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

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
// returns a nil document when the module has no directory (a bare in-memory
// resources.Module in a test) or has not exported any API contracts.
func loadContractCatalog(moduleDir string) (*contractCatalogDocument, error) {
	if moduleDir == "" {
		return nil, nil
	}
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
// and returns its identity, or nil when the module has no directory (a bare
// in-memory resources.Module in a test) or carries no package manifest.
func modulePackage(moduleDir string) (*InventoryPackage, error) {
	if moduleDir == "" {
		return nil, nil
	}
	manifest, err := composition.LoadPackageManifest(moduleDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load module package manifest: %w", err)
	}
	return &InventoryPackage{ID: manifest.ID, Version: manifest.Version}, nil
}
