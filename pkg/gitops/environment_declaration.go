package gitops

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
)

// --- The environment declaration a render bakes in ---
//
// Beside the workspace configuration groups (configuration_groups.go), a
// render bakes in the environment's own rows for the module's services: the
// service-config values it injects into each container, and the
// service-identity each workload runs as. Change those rows and publish
// nothing, and the delivery branch holds a tree rendered from a declaration
// that no longer exists, with nothing in the tree saying so: the workload keeps
// running on the value before the change and looks correctly configured.
//
// So every render records a digest of those rows as the environment declared
// them, over the services its inventory names, and a publish re-digests the
// same services against the environment as it is now and refuses a tree the
// declaration has moved past. The services are read from the inventory at
// both ends, so the two digests cover one set and a render is never refused
// for a service the publish could not see.

// serviceUnitNames is the name of every unit of Kind service an inventory
// records, managed ones (Kind service, Managed true) included: a managed
// service's identity rows are declared the same way, and a unit with no rows
// digests the same at both ends. Solution units are not among them.
func serviceUnitNames(units []InventoryUnit) []string {
	var names []string
	for _, unit := range units {
		if unit.Kind == UnitKindService && unit.Name != "" {
			names = append(names, unit.Name)
		}
	}
	sort.Strings(names)
	return names
}

// environmentDeclarationDigest digests, for the named services of a module,
// the environment's service-config values and service-identity as it declares
// them now. The result is a sha256 over one line per declared row, sorted, so
// two declarations that differ only in order digest the same.
func environmentDeclarationDigest(env *environments.Environment, module string, services []string) string {
	var lines []string
	for _, service := range services {
		for key, value := range env.ServiceConfig.ValuesFor(module, service) {
			lines = append(lines, fmt.Sprintf("service-config %s/%s %s=%s", module, service, key, value))
		}
		identity := env.WorkloadIdentityFor(module, service)
		if identity == nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("service-identity %s/%s kind=%s principal=%s", module, service, identity.Kind, identity.Principal))
		for key, value := range identity.Annotations {
			lines = append(lines, fmt.Sprintf("service-identity %s/%s annotation %s=%s", module, service, key, value))
		}
		for key, value := range identity.Labels {
			lines = append(lines, fmt.Sprintf("service-identity %s/%s label %s=%s", module, service, key, value))
		}
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// refuseStaleDeclaration refuses to publish a tree whose recorded environment
// declaration digest is not the environment's current one for the same
// services. A tree with no recorded digest was rendered by a CLI that did not
// record one and is held to nothing here: the hold cannot say what that
// render baked in, and the next render records it.
func refuseStaleDeclaration(env *environments.Environment, inventory *Inventory) error {
	if env == nil || inventory == nil || inventory.EnvironmentDeclarationDigest == "" {
		return nil
	}
	current := environmentDeclarationDigest(env, inventory.Module, serviceUnitNames(inventory.Units))
	if current == inventory.EnvironmentDeclarationDigest {
		return nil
	}
	return fmt.Errorf("the rendered tree of %s was rendered before environment %s's declaration for its services changed (service-config or service-identity rows of %s); render %s again for %s before publishing it",
		inventory.Module, env.Name, strings.Join(serviceUnitNames(inventory.Units), ", "), inventory.Module, env.Name)
}
