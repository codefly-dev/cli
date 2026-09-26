package generators

import (
	"sort"
	"strings"

	"github.com/codefly-dev/core/composition"
)

// ContractServicePackages returns, sorted, every proto package a protobuf
// contract entry declares services in. An entry's Package names only its
// primary package; a service that serves a second API in another package (a
// generic receipt service beside its own, say) records those services in
// Services by their fully-qualified names, so anything that walks an entry's
// services by package has to walk each of these. An entry that records no
// services falls back to its Package, the shape every single-package catalog
// has always had.
func ContractServicePackages(endpoint *composition.APIContractEndpoint) []string {
	seen := map[string]bool{}
	var packages []string
	for i := range endpoint.Services {
		service := &endpoint.Services[i]
		pkg, ok := strings.CutSuffix(service.FullName, "."+service.Name)
		if !ok || seen[pkg] {
			continue
		}
		seen[pkg] = true
		packages = append(packages, pkg)
	}
	if len(packages) == 0 && endpoint.Package != "" {
		return []string{endpoint.Package}
	}
	sort.Strings(packages)
	return packages
}
