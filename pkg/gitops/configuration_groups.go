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
	"github.com/codefly-dev/cli/pkg/modulecontract"
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
// So every render records, per group its services and its contract's slots
// consume, a digest of the group as the environment provided it. The rule is
// enforced at PUBLISH, where each refusal has one action that satisfies it:
//
//   - a module whose recorded digest is not the composition's current value
//     was rendered before the group changed, and is refused until rendered
//     again;
//   - a sibling consumer whose local tree records another digest has not been
//     rendered since the group changed, and this publish is refused until it
//     is — the render that lifts the refusal is the sibling's, which the
//     publish names.
//
// A render never refuses on a sibling's account. It did once, symmetrically,
// and that deadlocked: with A and B both rendered against the old value,
// rendering A was refused because B's tree was stale and rendering B because
// A's still was, and no sequence of renders got out. The render reports the
// stale siblings instead, so the operator renders them next.
//
// What a publish cannot enforce is the delivery base: a consumer published
// earlier at the old value is stale there until its own publish, and refusing
// this one for it would be the same deadlock one repository over. So the base
// branch's stale consumers are reported in the plan, by name, and are the next
// publish to run.

// workspaceConfigurationDigests digests, per group the module's services
// consume, the group as the environment provides it. The digest covers each
// key and its public value, and the key alone of a secret value: the rendered
// tree carries a secret only as a reference, so the value is not part of what
// the tree bakes in, while the key's presence is.
func workspaceConfigurationDigests(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, services []*resources.Service, slotGroups []string) (map[string]string, error) {
	consumed := map[string]struct{}{}
	for _, service := range services {
		for _, group := range service.WorkspaceConfigurationDependencies {
			consumed[group] = struct{}{}
		}
	}
	// A contract slot {from: <group>/<key>} is consumed into a delivered
	// document, so its group is baked in exactly as a service's is.
	for _, group := range slotGroups {
		consumed[group] = struct{}{}
	}
	if len(consumed) == 0 {
		return nil, nil
	}
	return digestProvidedGroups(ctx, workspace, env, consumed)
}

// digestProvidedGroups digests the named groups as the environment provides
// them now; a group the environment does not provide is absent from the
// result.
func digestProvidedGroups(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, consumed map[string]struct{}) (map[string]string, error) {
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

// recordedGroupsNow re-digests the groups a rendered tree recorded, as the
// environment provides them now: what a publish holds the render to. It reads
// no service, so a publish needs only the tree and the composition.
func recordedGroupsNow(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, recorded map[string]string) (map[string]string, error) {
	if len(recorded) == 0 {
		return nil, nil
	}
	consumed := make(map[string]struct{}, len(recorded))
	for group := range recorded {
		consumed[group] = struct{}{}
	}
	return digestProvidedGroups(ctx, workspace, env, consumed)
}

// currentGroupDigests digests the groups a module consumes as the environment
// provides them NOW: its services' declared dependencies and its contract's
// slots. A render records this; a publish re-digests the recorded groups and
// refuses a render the composition has moved past.
func currentGroupDigests(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, module *resources.Module) (map[string]string, error) {
	services := make([]*resources.Service, 0, len(module.ServiceReferences))
	for _, reference := range module.ServiceReferences {
		service, err := module.LoadServiceFromName(ctx, reference.Name)
		if err != nil {
			return nil, fmt.Errorf("load service %s: %w", reference.Name, err)
		}
		services = append(services, service)
	}
	var slotGroups []string
	contract, err := modulecontract.Load(module.Dir())
	switch {
	case err == nil:
		slotGroups = contract.SlotGroups()
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	return workspaceConfigurationDigests(ctx, workspace, env, services, slotGroups)
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
		"workspace configuration groups consumed by %s changed since their other consumers were rendered for %s: %s; render those modules, so the environment does not deliver two values of one group",
		err.Module, err.Environment, strings.Join(parts, "; "))
}

// staleGroupConsumers compares the group digests a render recorded with the
// ones every sibling module tree under deployments/modules recorded for the
// same environment, and returns, per group they both consume, the siblings
// whose digest differs. A sibling tree that cannot be read is an error, not
// an absence: an unreadable inventory is no evidence that its module agrees.
func staleGroupConsumers(workspaceDir, module, environment string, digests map[string]string) (map[string][]string, error) {
	if len(digests) == 0 {
		return nil, nil
	}
	modulesDir := filepath.Join(workspaceDir, "deployments", "modules")
	entries, err := os.ReadDir(modulesDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list rendered modules: %w", err)
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
			return nil, fmt.Errorf("the rendered tree of module %s cannot be read, so whether it agrees about the groups %s consumes cannot be told: %w", entry.Name(), module, err)
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
	for group := range stale {
		sort.Strings(stale[group])
	}
	return stale, nil
}

// refuseStaleGroupConsumers refuses a publish while a sibling consumer's local
// tree records another value of a group this module consumes: the render that
// lifts it is the sibling's, named in the error.
func refuseStaleGroupConsumers(workspaceDir, module, environment string, digests map[string]string) error {
	stale, err := staleGroupConsumers(workspaceDir, module, environment, digests)
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		return nil
	}
	return &StaleGroupConsumersError{Module: module, Environment: environment, Stale: stale}
}

// refuseStaleRender refuses a publish of a tree rendered against a value a
// consumed group no longer has: the composition moved after the render, and
// the fix is to render again. Recorded groups the composition no longer
// provides, and provided groups the render did not record, are each a
// difference too.
func refuseStaleRender(module, environment string, recorded, current map[string]string) error {
	var moved []string
	for group, digest := range current {
		if recorded[group] != digest {
			moved = append(moved, group)
		}
	}
	for group := range recorded {
		if _, provided := current[group]; !provided {
			moved = append(moved, group)
		}
	}
	if len(moved) == 0 {
		return nil
	}
	sort.Strings(moved)
	return fmt.Errorf("the rendered tree of %s bakes in a value of the workspace configuration groups %s that %s no longer provides; render %s again before publishing it",
		module, strings.Join(moved, ", "), environment, module)
}

// staleBaseConsumers names, per group this module consumes, the consumers the
// delivery base branch holds at another digest: published before the group
// changed, and the next publish to run. Reported, never refused — refusing
// would hold every consumer's publish on every other's.
// staleBaseConsumer is a module delivered on the base branch whose render
// bakes in a workspace configuration group at a digest the publishing module
// no longer agrees with.
type staleBaseConsumer struct {
	Module, Group string
	// Current is the digest the publishing module bakes in now.
	Current string
}

// describeStaleConsumers names stale consumers the way the plan reports them.
func describeStaleConsumers(stale []staleBaseConsumer) []string {
	named := make([]string, 0, len(stale))
	for _, consumer := range stale {
		named = append(named, fmt.Sprintf("%s (group %s)", consumer.Module, consumer.Group))
	}
	return named
}

// refuseStaleBaseConsumers refuses to publish past a consumer the base branch
// delivers at a stale digest unless this workspace holds a current render of
// it: then the base branch would carry a consumer of a value its provider no
// longer gives, with nothing queued to replace it. A current local render is
// what the render step reports as needed, so the refusal is lifted by
// rendering the named module beside this one and publishing both.
func refuseStaleBaseConsumers(workspaceDir, environment, baseBranch string, stale []staleBaseConsumer) error {
	var unrendered []string
	for _, consumer := range stale {
		inventory, err := LoadInventory(filepath.Join(workspaceDir, "deployments", "modules", consumer.Module))
		if errors.Is(err, fs.ErrNotExist) {
			unrendered = append(unrendered, fmt.Sprintf("%s (group %s)", consumer.Module, consumer.Group))
			continue
		}
		if err != nil {
			return fmt.Errorf("the rendered tree of module %s cannot be read, so whether it bakes in group %s as provided now is unknown: %w", consumer.Module, consumer.Group, err)
		}
		if inventory.Environment != environment || inventory.WorkspaceConfigurationDigests[consumer.Group] != consumer.Current {
			unrendered = append(unrendered, fmt.Sprintf("%s (group %s)", consumer.Module, consumer.Group))
		}
	}
	if len(unrendered) == 0 {
		return nil
	}
	sort.Strings(unrendered)
	return fmt.Errorf("the modules %s delivered on %s bake in a value of a workspace configuration group this render changes, and this workspace holds no current render of them; render them for %s and publish them with this one", strings.Join(unrendered, ", "), baseBranch, environment)
}

func staleBaseConsumers(ctx context.Context, repo, baseBranch, pathRoot, module string, digests map[string]string) ([]staleBaseConsumer, error) {
	if len(digests) == 0 {
		return nil, nil
	}
	ref := "refs/remotes/origin/" + baseBranch
	modulesPath := filepath.ToSlash(filepath.Join(pathRoot, "deployments", "modules"))
	listing, err := gitCommand(ctx, repo, "ls-tree", "--name-only", ref+":"+modulesPath)
	if err != nil {
		// No modules directory on the base branch is the first publish of the
		// composition; nothing is delivered to be stale.
		return nil, nil
	}
	var stale []staleBaseConsumer
	for _, name := range strings.Split(strings.TrimSpace(listing), "\n") {
		name = strings.TrimSpace(name)
		if name == "" || name == module {
			continue
		}
		data, err := gitCommandBytes(ctx, repo, "show", ref+":"+modulesPath+"/"+name+"/"+InventoryFilename)
		if err != nil {
			continue
		}
		inventory, err := decodeInventory(data, name)
		if err != nil {
			return nil, fmt.Errorf("the delivered inventory of module %s on %s cannot be read: %w", name, baseBranch, err)
		}
		for group, digest := range digests {
			if other, consumed := inventory.WorkspaceConfigurationDigests[group]; consumed && other != digest {
				stale = append(stale, staleBaseConsumer{Module: name, Group: group, Current: digest})
			}
		}
	}
	sort.Slice(stale, func(i, j int) bool {
		if stale[i].Module != stale[j].Module {
			return stale[i].Module < stale[j].Module
		}
		return stale[i].Group < stale[j].Group
	})
	return stale, nil
}
