package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/resources"
)

// --- Workspace configuration groups a render consumes ---
//
// A workspace configuration group is one value however many modules read it,
// and a render bakes the value it read into the tree it delivers. Change the
// group and render only one of its consumers, and the delivery repository holds
// two modules that disagree about one value, with nothing in either tree saying
// so: the one that was not re-rendered keeps serving the old value and looks
// correctly configured.
//
// So every render records, per group its services consume, a digest of the
// group as the environment provided it, and refuses to replace its tree while a
// sibling module rendered for the same environment records a different digest
// for a group they both consume. The check runs at render, over the module
// trees beside this one, and deliberately not at publish: a publish-time check
// against the delivery repository would refuse whichever consumer publishes
// first, for ever, because the other is not published yet.

// workspaceConfigurationDigests digests, per group the module's services
// consume, the group as the environment provides it. The digest covers each
// key and its public value, and the key alone of a secret value: the rendered
// tree carries a secret only as a reference, so the value is not part of what
// the tree bakes in, while the key's presence is.
func workspaceConfigurationDigests(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, services []*resources.Service) (map[string]string, error) {
	consumed := map[string]struct{}{}
	for _, service := range services {
		for _, group := range service.WorkspaceConfigurationDependencies {
			consumed[group] = struct{}{}
		}
	}
	if len(consumed) == 0 {
		return nil, nil
	}
	provided, err := configurations.ReadWorkspaceConfigurations(ctx, workspace, env.Runtime())
	if err != nil {
		return nil, fmt.Errorf("read the workspace configurations this render bakes in: %w", err)
	}
	digests := make(map[string]string, len(consumed))
	for _, info := range provided.Infos {
		if _, wanted := consumed[info.Name]; !wanted {
			continue
		}
		lines := make([]string, 0, len(info.ConfigurationValues))
		for _, value := range info.ConfigurationValues {
			if value.Secret {
				lines = append(lines, value.Key+"=<secret>")
				continue
			}
			lines = append(lines, value.Key+"="+value.Value)
		}
		sort.Strings(lines)
		sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
		digests[info.Name] = "sha256:" + hex.EncodeToString(sum[:])
	}
	return digests, nil
}

// StaleGroupConsumersError reports sibling modules whose rendered trees bake in
// another value of a workspace configuration group this render consumes.
type StaleGroupConsumersError struct {
	Module      string
	Environment string
	// Stale maps a group to the modules rendered against another value of it.
	Stale map[string][]string
}

func (err *StaleGroupConsumersError) Error() string {
	groups := make([]string, 0, len(err.Stale))
	for group := range err.Stale {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	parts := make([]string, 0, len(groups))
	for _, group := range groups {
		parts = append(parts, fmt.Sprintf("%s (rendered by %s)", group, strings.Join(err.Stale[group], ", ")))
	}
	return fmt.Sprintf(
		"workspace configuration groups consumed by %s changed since their other consumers were rendered for %s: %s; render those modules too, so the environment does not deliver two values of one group",
		err.Module, err.Environment, strings.Join(parts, "; "))
}

// refuseStaleGroupConsumers compares the group digests this render recorded
// with the ones every sibling module tree under deployments/modules recorded
// for the same environment, and refuses when any group they both consume
// digests differently.
func refuseStaleGroupConsumers(workspaceDir, module, environment string, digests map[string]string) error {
	if len(digests) == 0 {
		return nil
	}
	modulesDir := filepath.Join(workspaceDir, "deployments", "modules")
	entries, err := os.ReadDir(modulesDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list rendered modules: %w", err)
	}
	stale := map[string][]string{}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == module {
			continue
		}
		inventory, err := LoadInventory(filepath.Join(modulesDir, entry.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			// A sibling tree this CLI cannot read is not evidence either way;
			// its own render will refuse it.
			continue
		}
		if inventory.Environment != environment {
			continue
		}
		for group, digest := range digests {
			other, consumed := inventory.WorkspaceConfigurationDigests[group]
			if consumed && other != digest {
				stale[group] = append(stale[group], inventory.Module)
			}
		}
	}
	if len(stale) == 0 {
		return nil
	}
	for group := range stale {
		sort.Strings(stale[group])
	}
	return &StaleGroupConsumersError{Module: module, Environment: environment, Stale: stale}
}
