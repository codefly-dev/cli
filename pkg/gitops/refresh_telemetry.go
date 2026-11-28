package gitops

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
)

// refreshDeliveredTelemetry runs on the private, validated copy of the delivered
// revision which publication subsequently snapshots and signs.
func refreshDeliveredTelemetry(ctx context.Context, root string, env *environments.Environment, inventory *Inventory) error {
	if env.Telemetry == nil || inventory.Environment != env.Name {
		return errors.New("telemetry refresh needs a declared receiver for the delivered environment")
	}
	if err := env.Telemetry.Validate(); err != nil {
		return err
	}
	tree, err := newRenderRoot(root)
	if err != nil {
		return err
	}
	defer tree.Close()
	changed := false
	for index := range inventory.Units {
		unit := &inventory.Units[index]
		if unit.Kind != UnitKindService || unit.Path == "" {
			continue
		}
		unitDir := filepath.Join(tree.path, filepath.FromSlash(unit.Path))
		updated, refreshErr := refreshServiceTelemetry(ctx, tree, unitDir, env, unit.Module, unit.Name)
		if refreshErr != nil {
			return fmt.Errorf("refresh telemetry for %s/%s: %w", unit.Module, unit.Name, refreshErr)
		}
		changed = changed || updated
	}
	if !changed {
		return ErrNoChanges
	}
	actual, err := buildInventory(tree.path, &RenderOptions{renderRoot: tree})
	if err != nil {
		return err
	}
	inventory.Files, inventory.Digest = actual.Files, actual.Digest
	if err := writeCanonicalInventory(filepath.Join(root, InventoryFilename), inventory); err != nil {
		return err
	}
	return ValidateRenderedTree(root, inventory.AppProject, true)
}

func refreshServiceTelemetry(ctx context.Context, tree *renderRoot, unitDir string, env *environments.Environment, module, service string) (bool, error) {
	carriers := env.TelemetryCarriers(module, service)
	files, err := configurationSourcesInTree(tree, unitDir, env.Name)
	if err != nil {
		return false, err
	}
	changed := map[uintptr]bool{}
	for _, docs := range files {
		for _, doc := range docs {
			if doc.group != "" || doc.kind != kindConfigMap {
				continue
			}
			data := mapField(doc.value, dataKey)
			if data[resources.ServicePrefix] != service {
				continue
			}
			const endpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
			current, exists := data[endpoint]
			if !exists {
				continue
			}
			for key, want := range carriers {
				if key != endpoint && data[key] != want {
					return false, fmt.Errorf("existing telemetry carrier %s differs from delivered service identity", key)
				}
			}
			if current == carriers[endpoint] {
				continue
			}
			data[endpoint] = carriers[endpoint]
			changed[reflect.ValueOf(doc.value).Pointer()] = true
		}
	}
	if len(changed) == 0 {
		return false, nil
	}
	overlay, err := renderOverlay(tree, unitDir, env.Name)
	if err != nil {
		return false, err
	}
	manifests, err := overlayManifestsInTree(tree, overlay)
	if err != nil {
		return false, err
	}
	prior, err := deliveredConfigurationDigests(manifests)
	if err != nil {
		return false, err
	}
	if err := writeChangedConfigurationFiles(ctx, tree, slices.Sorted(maps.Keys(files)), files, changed); err != nil {
		return false, err
	}
	if err := validateProjectedInjectionInTree(tree, unitDir, service, env, serviceInjection{Public: carriers}); err != nil {
		return false, err
	}
	if err := refreshConfigurationDigest(tree, unitDir, env.Name, prior); err != nil {
		return false, err
	}
	return true, nil
}

// A receiver-only change cannot replace someone else's in-flight delivery,
// even one changing only paths inside the module it is publishing.
func holdTelemetryPromotionBranch(base, existing, replaces string) error {
	if replaces != "" || (existing != "" && existing != base) {
		return errors.New("already carries a separate change; review its open promotion or use a fresh promotion branch")
	}
	return nil
}
