package gitops

// A render is a claim about a cell: the cell file is regenerated whole and a
// cell admits only what it names, so a workload, a namespace or a consumer
// absent from it is withdrawn. A publication promotes a render, so before it
// does, the workspace's cell file is held to the one the delivery base branch
// carries, and a removal it does not name is refused: a cell file derived
// from a checkout missing some modules' renders would otherwise remove those
// modules from admission without anyone having said so.

import (
	"context"
	"fmt"
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
// carries, unless every removal concerns a module allowed names. It returns
// the removals allowed, for the plan to show. A publication carrying no cell
// file (publicationCellFile), or a base with none, has nothing to hold.
//
// It is the cell file the publication CARRIES that is held — the same bytes,
// read once — so the file delivered and the file held can never be two
// different files, and the hold runs before anything is carried.
func holdCellToBase(ctx context.Context, cell *publicationCell, env *environments.Environment, repo, baseBranch, baseRevision string, allowed []string) ([]string, error) {
	if cell == nil {
		return nil, nil
	}
	path, next := cell.source, cell.file
	baseData, err := gitCommandBytes(ctx, repo, "show", baseRevision+":"+cell.delivered)
	if err != nil {
		if gitSaysAbsent(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s's cell file %s: %w", baseBranch, cell.delivered, err)
	}
	base, err := cellfile.Parse(baseData)
	if err != nil {
		return nil, fmt.Errorf("%s's cell file %s cannot be read: %w", baseBranch, cell.delivered, err)
	}
	var refused, admitted []string
	unnamed := map[string]bool{}
	for _, removal := range cellRemovals(base, next) {
		if slices.ContainsFunc(removal.modules, func(module string) bool { return slices.Contains(allowed, module) }) {
			admitted = append(admitted, removal.what)
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
		return nil, fmt.Errorf("environment %s's cell file %s removes from the cell what %s's admits, and says nothing of it: %s. Publishing it would withdraw those from the cell's admission set. If a module's render is missing from this checkout, render it (`codefly deploy gitops render --all --env %s`); if the removal is intended — a module the workspace no longer composes, a dependency dropped — name each module it concerns with --allow-cell-removal (%s)",
			env.Name, path, baseBranch, strings.Join(refused, "; "), env.Name, strings.Join(modules, ", "))
	}
	return admitted, nil
}
