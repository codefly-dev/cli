package gitops

// A render is a claim about a cell: the cell file is regenerated whole and a
// cell admits only what it names, so a workload, a namespace or a consumer
// absent from it is withdrawn. A publication promotes a render, so before it
// does, the workspace's cell file is held to the one the delivery base branch
// carries, and a removal nothing explains is refused: a cell file derived
// from a checkout missing some modules' renders would otherwise remove those
// modules from admission without anyone having said so. A removal the
// composition explains — the module left `modules:` — is carried without
// more, and the module's delivered tree goes with it: the cell no longer
// admits anything of it, and the ApplicationSet that tree carries would keep
// stamping Applications whose every pod the cell refuses.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops/cellfile"
)

// cellRemoval is one thing the base cell file admits that the next one does
// not, and the modules it concerns: the namespace's module, and for a
// consumer, the consumer's module too.
type cellRemoval struct {
	modules []string
	what    string
}

// cellRemovals names every namespace, workload, endpoint and endpoint
// consumer base carries that next does not, in base's order.
func cellRemovals(base, next *cellfile.File) []cellRemoval {
	var removals []cellRemoval
	for n := range base.Namespaces {
		was := &base.Namespaces[n]
		index := slices.IndexFunc(next.Namespaces, func(candidate cellfile.Namespace) bool { return candidate.Name == was.Name })
		if index < 0 {
			removals = append(removals, cellRemoval{modules: []string{was.Module}, what: fmt.Sprintf("namespace %s (module %s, %d workload(s))", was.Name, was.Module, len(was.Workloads))})
			continue
		}
		is := &next.Namespaces[index]
		for w := range was.Workloads {
			workload := &was.Workloads[w]
			found := slices.IndexFunc(is.Workloads, func(candidate cellfile.Workload) bool {
				return candidate.Name == workload.Name && candidate.Kind == workload.Kind
			})
			if found < 0 {
				removals = append(removals, cellRemoval{modules: []string{was.Module}, what: fmt.Sprintf("workload %s/%s %s", was.Name, workload.Kind, workload.Name)})
				continue
			}
			removals = append(removals, endpointRemovals(was, workload, &is.Workloads[found])...)
		}
	}
	return removals
}

// endpointRemovals names the endpoints of one workload, and the consumers of
// each, that the next file no longer carries.
func endpointRemovals(namespace *cellfile.Namespace, was, is *cellfile.Workload) []cellRemoval {
	var removals []cellRemoval
	for e := range was.Endpoints {
		endpoint := &was.Endpoints[e]
		where := fmt.Sprintf("%s/%s/%s", namespace.Name, was.Name, endpoint.Name)
		found := slices.IndexFunc(is.Endpoints, func(candidate cellfile.Endpoint) bool { return candidate.Name == endpoint.Name })
		if found < 0 {
			removals = append(removals, cellRemoval{modules: []string{namespace.Module}, what: "endpoint " + where})
			continue
		}
		for _, consumer := range endpoint.Consumers {
			if slices.Contains(is.Endpoints[found].Consumers, consumer) {
				continue
			}
			consumerModule, _, _ := strings.Cut(consumer, "/")
			removals = append(removals, cellRemoval{modules: []string{namespace.Module, consumerModule}, what: fmt.Sprintf("consumer %s of %s", consumer, where)})
		}
	}
	return removals
}

// deliveryCellPath is the environment's cell file in the delivery
// repository: beside the modules tree, as the render lays it out in the
// workspace (CellPath).
func deliveryCellPath(gitopsPath, environment string) string {
	if gitopsPath == "" {
		gitopsPath = DefaultGitopsPath
	}
	return filepath.ToSlash(filepath.Join(gitopsPath, cellsDir, environment, cellfile.FileName))
}

// holdCellToBase refuses the publication when the cell file it carries for a
// hosted environment removes anything the delivery base revision's file
// carries, unless the removal is explained: it concerns a module the
// composition no longer declares (declared is the composition's module set),
// or a module allowed names. It returns the removals allowed by name, and the
// removals the composition explains, for the plan to show. A publication
// carrying no cell file (publicationCellFile), or a base with none, has
// nothing to hold.
//
// A removal the composition explains needs no naming: the flag confirms a
// removal nothing else accounts for — a module still composed whose render is
// missing from the checkout, a dependency dropped — where a module gone from
// `modules:` is the removal's own statement of intent, and asking for it again
// made every publication after such a change refuse until someone typed the
// module's name back in.
//
// It is the cell file the publication CARRIES that is held — the same bytes,
// read once — so the file delivered and the file held can never be two
// different files, and the hold runs before anything is carried.
func holdCellToBase(ctx context.Context, cell *publicationCell, env *environments.Environment, repo, baseBranch, baseRevision string, allowed []string, declared map[string]bool) (named, undeclared []string, err error) {
	if cell == nil {
		return nil, nil, nil
	}
	path, next := cell.source, cell.file
	baseData, err := gitCommandBytes(ctx, repo, "show", baseRevision+":"+cell.delivered)
	if err != nil {
		if gitSaysAbsent(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read %s's cell file %s: %w", baseBranch, cell.delivered, err)
	}
	base, err := cellfile.Parse(baseData)
	if err != nil {
		return nil, nil, fmt.Errorf("%s's cell file %s cannot be read: %w", baseBranch, cell.delivered, err)
	}
	var refused []string
	unnamed := map[string]bool{}
	for _, removal := range cellRemovals(base, next) {
		if gone := slices.IndexFunc(removal.modules, func(module string) bool { return !declared[module] }); gone >= 0 {
			undeclared = append(undeclared, fmt.Sprintf("%s: the composition no longer declares module %s", removal.what, removal.modules[gone]))
			continue
		}
		if slices.ContainsFunc(removal.modules, func(module string) bool { return slices.Contains(allowed, module) }) {
			named = append(named, removal.what)
			continue
		}
		refused = append(refused, removal.what)
		unnamed[removal.modules[len(removal.modules)-1]] = true
	}
	if len(refused) > 0 {
		modules := make([]string, 0, len(unnamed))
		for module := range unnamed {
			modules = append(modules, module)
		}
		slices.Sort(modules)
		return nil, nil, fmt.Errorf("environment %s's cell file %s removes from the cell what %s's admits, and says nothing of it: %s. Publishing it would withdraw those from the cell's admission set. If a module's render is missing from this checkout, render it (`codefly deploy gitops render --all --env %s`); if the removal is intended — a dependency dropped, a service withdrawn — name each module it concerns with --allow-cell-removal (%s)",
			env.Name, path, baseBranch, strings.Join(refused, "; "), env.Name, strings.Join(modules, ", "))
	}
	return named, undeclared, nil
}

// undeclaredDeliveries names the module trees the delivery repository holds
// under the environment's modules path, as the promotion clone stands, whose
// module the composition no longer declares: delivered once, and left behind
// when the module left `modules:`. A tree is a delivery when it carries a
// render record (InventoryFilename); anything else under that path is not this
// package's to judge. The module being published is never among them: a
// publish of a module the composition does not compose is refused long before
// this. Paths are relative to the repository, slash-separated.
func undeclaredDeliveries(repo, gitopsPath, module string, declared map[string]bool) ([]string, error) {
	modulesPath := deliveryModulesPath(gitopsPath)
	modulesDir, err := confinedJoin(repo, modulesPath)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(modulesDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list the delivered module trees under %s: %w", modulesPath, err)
	}
	var stale []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == module || declared[entry.Name()] {
			continue
		}
		if _, err := os.Stat(filepath.Join(modulesDir, entry.Name(), InventoryFilename)); err != nil {
			continue
		}
		stale = append(stale, deliveryModulePath(gitopsPath, entry.Name()))
	}
	return stale, nil
}

// removeUndeclaredDeliveries deletes from the promotion clone the delivered
// trees of modules the composition no longer declares (undeclaredDeliveries),
// and answers their paths for the publication to own, stage and show. A
// forward publication to a hosted environment carries the composition's claim
// whole — the cell file it carries no longer admits those modules — so it is
// the one that removes them; a rollback restores one module's tree and claims
// nothing about the rest, and an environment with no host has no cell whose
// claim the trees contradict. Nothing is staged here: the removal reaches the
// promotion only with everything else the publication writes, after the hold.
func removeUndeclaredDeliveries(repo, gitopsPath, module string, env *environments.Environment, restoreRevision string, declared map[string]bool) ([]string, error) {
	if env == nil || env.Host == nil || restoreRevision != "" {
		return nil, nil
	}
	deliveries, err := undeclaredDeliveries(repo, gitopsPath, module, declared)
	if err != nil {
		return nil, err
	}
	for _, delivery := range deliveries {
		target, err := confinedJoin(repo, delivery)
		if err != nil {
			return nil, err
		}
		if err = os.RemoveAll(target); err != nil {
			return nil, fmt.Errorf("remove the delivered tree %s of a module the composition no longer declares: %w", delivery, err)
		}
	}
	return deliveries, nil
}
