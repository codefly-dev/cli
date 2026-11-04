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
	"github.com/codefly-dev/core/resources"
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

// compositionClaim is what a publication may remove from the cell and the
// delivery on the composition's word alone: the modules the composition no
// longer declares — provided this checkout's composition has moved FORWARD
// from the one that rendered the delivery base's cell file (advanced). A
// checkout that never had a module and one that dropped it derive the same
// cell file, and only the history tells them apart: a checkout that contains
// the commit the base's file came from has seen the composition that named
// the module, so its absence is a removal; one that does not is stale, and
// its publication keeps needing --allow-cell-removal, as every withdrawal
// nobody stated does.
type compositionClaim struct {
	// module is the one being published: never a removal of its own.
	module string
	// declared is the composition's module set (composedModules).
	declared map[string]bool
	// advanced is whether the checkout descends from revision; revision is the
	// commit it had to contain, empty when the base names none.
	advanced bool
	revision string
}

// newCompositionClaim judges the checkout against the delivery base. The
// commit the checkout must contain is the base revision itself whenever the
// workspace repository and the delivery repository are one — the workspace
// holds the base commit, or the delivery clone holds the workspace revision
// the base's cell file says it was rendered from (cellSource) — because that
// revision is the render's PARENT and a `modules:` edit rides in the render's
// own commit, so a checkout forked at the parent has not seen it; an unfetched
// checkout of one repository is therefore not advanced, and the hold says
// which commit it lacks. Only when the two repositories are separate (the
// clone does not hold the source revision) is the source revision what the
// checkout must contain. A base with no cell file, or one naming no source
// while the workspace lacks the base commit, leaves nothing to descend from.
func newCompositionClaim(ctx context.Context, workspace *resources.Workspace, clone, module, baseRevision string, base *cellfile.File) *compositionClaim {
	claim := &compositionClaim{module: module, declared: composedModules(workspace)}
	if base == nil {
		return claim
	}
	dir := workspace.Dir()
	head, err := gitCommand(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return claim
	}
	source := ""
	if base.Source != nil {
		source = base.Source.Revision
	}
	switch {
	case gitHasCommit(ctx, dir, baseRevision), source != "" && gitHasCommit(ctx, clone, source):
		claim.revision = baseRevision
	case source != "":
		claim.revision = source
	default:
		return claim
	}
	claim.advanced = gitIsAncestor(ctx, dir, claim.revision, head)
	return claim
}

// gitHasCommit reports whether the repository holds the commit.
func gitHasCommit(ctx context.Context, repo, revision string) bool {
	_, err := gitCommand(ctx, repo, "cat-file", "-e", revision+"^{commit}")
	return err == nil
}

// undeclared reports a module the composition does not declare, other than
// the one being published.
func (claim *compositionClaim) undeclared(module string) bool {
	return module != claim.module && !claim.declared[module]
}

// explains reports a removal the composition accounts for: the module is no
// longer declared, by a checkout that has seen it declared.
func (claim *compositionClaim) explains(module string) bool {
	return claim.advanced && claim.undeclared(module)
}

// owns answers, for the promotion-branch guard, whether a delivery-repository
// path lies in the delivered tree of a module the composition no longer
// declares. A promotion of this module owns those trees whether or not the
// clone still holds them: the branch it continues, or the stale tip it
// replaces, carries this module's own earlier removal of such a tree, and the
// base may since have taken it, so neither tree says what the branch removed.
func (claim *compositionClaim) owns(gitopsPath string) func(path string) bool {
	prefix := deliveryModulesPath(gitopsPath) + "/"
	return func(path string) bool {
		rest, under := strings.CutPrefix(path, prefix)
		if !under {
			return false
		}
		name, _, _ := strings.Cut(rest, "/")
		return name != "" && claim.undeclared(name)
	}
}

// compositionBase is where a publication's composition claim is judged: the
// promotion clone, the delivery base it was cut from, and the publication.
type compositionBase struct {
	repo, gitopsPath, branch, revision string
	module, restoreRevision            string
}

// judgeCompositionRemovals is the one step a publication takes on the
// composition's word: it reads the base's cell file, judges the checkout's
// claim against it and removes from the clone the delivered trees that claim
// explains, answering all three for the hold, the branch guard and the plan.
func judgeCompositionRemovals(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, cell *publicationCell, base *compositionBase) (*cellfile.File, *compositionClaim, []string, error) {
	baseCell, err := baseCellFile(ctx, cell, base.repo, base.branch, base.revision)
	if err != nil {
		return nil, nil, nil, err
	}
	claim := newCompositionClaim(ctx, workspace, base.repo, base.module, base.revision, baseCell)
	removed, err := removeUndeclaredDeliveries(base.repo, base.gitopsPath, env, base.restoreRevision, claim)
	if err != nil {
		return nil, nil, nil, err
	}
	return baseCell, claim, removed, nil
}

// baseCellFile reads the cell file the delivery base revision carries at the
// path the publication delivers to, nil when the base has none. A publication
// carrying no cell file is held to nothing, and reads none.
func baseCellFile(ctx context.Context, cell *publicationCell, repo, baseBranch, baseRevision string) (*cellfile.File, error) {
	if cell == nil {
		return nil, nil
	}
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
	return base, nil
}

// holdCellToBase refuses the publication when the cell file it carries for a
// hosted environment removes anything the delivery base revision's file (base)
// carries, unless the removal is explained: the composition no longer
// declares the module it concerns and the checkout has seen it declared
// (compositionClaim), or a module allowed names. It returns the removals
// allowed by name, and the removals the composition explains, for the plan to
// show. A publication carrying no cell file, or a base with none, has nothing
// to hold.
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
func holdCellToBase(cell *publicationCell, env *environments.Environment, baseBranch string, base *cellfile.File, allowed []string, claim *compositionClaim) (named, undeclared []string, err error) {
	if cell == nil || base == nil {
		return nil, nil, nil
	}
	path, next := cell.source, cell.file
	var refused, stale []string
	unnamed := map[string]bool{}
	for _, removal := range cellRemovals(base, next) {
		if gone := slices.IndexFunc(removal.modules, claim.explains); gone >= 0 {
			undeclared = append(undeclared, fmt.Sprintf("%s: the composition no longer declares module %s", removal.what, removal.modules[gone]))
			continue
		}
		if slices.ContainsFunc(removal.modules, func(module string) bool { return slices.Contains(allowed, module) }) {
			named = append(named, removal.what)
			continue
		}
		refused = append(refused, removal.what)
		unnamed[removal.modules[len(removal.modules)-1]] = true
		if gone := slices.IndexFunc(removal.modules, claim.undeclared); gone >= 0 && !slices.Contains(stale, removal.modules[gone]) {
			stale = append(stale, removal.modules[gone])
		}
	}
	if len(refused) == 0 {
		return named, undeclared, nil
	}
	modules := make([]string, 0, len(unnamed))
	for module := range unnamed {
		modules = append(modules, module)
	}
	slices.Sort(modules)
	message := fmt.Sprintf("environment %s's cell file %s removes from the cell what %s's admits, and says nothing of it: %s. Publishing it would withdraw those from the cell's admission set. If a module's render is missing from this checkout, render it (`codefly deploy gitops render --all --env %s`); if the removal is intended — a dependency dropped, a service withdrawn — name each module it concerns with --allow-cell-removal (%s)",
		env.Name, path, baseBranch, strings.Join(refused, "; "), env.Name, strings.Join(modules, ", "))
	if len(stale) > 0 {
		slices.Sort(stale)
		if claim.revision == "" {
			message += fmt.Sprintf(". This checkout's composition does not declare %s, but %s's cell file names no revision it was rendered from, so whether the composition removed %s or never had it cannot be told from here", strings.Join(stale, ", "), baseBranch, strings.Join(stale, ", "))
		} else {
			message += fmt.Sprintf(". This checkout's composition does not declare %s, but the checkout does not contain %s, which %s's cell file comes from, so the removal may be a stale checkout's rather than the composition's: bring the checkout up to date and render again, or name it", strings.Join(stale, ", "), claim.revision, baseBranch)
		}
	}
	return nil, nil, errors.New(message)
}

// undeclaredDeliveries names the module trees the delivery repository holds
// under the environment's modules path, as the promotion clone stands, whose
// removal the composition explains (compositionClaim): delivered once, and
// left behind when the module left `modules:`. A tree is a delivery when it
// carries a render record (InventoryFilename); anything else under that path
// is not this package's to judge. Paths are relative to the repository,
// slash-separated.
func undeclaredDeliveries(repo, gitopsPath string, claim *compositionClaim) ([]string, error) {
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
		if !entry.IsDir() || !claim.explains(entry.Name()) {
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
// and answers their paths for the publication to stage and show. A forward
// publication to a hosted environment carries the composition's claim whole —
// the cell file it carries no longer admits those modules — so it is the one
// that removes them; a rollback restores one module's tree and claims nothing
// about the rest, and an environment with no host has no cell whose claim the
// trees contradict. Nothing is staged here: the removal reaches the promotion
// only with everything else the publication writes, after the hold.
func removeUndeclaredDeliveries(repo, gitopsPath string, env *environments.Environment, restoreRevision string, claim *compositionClaim) ([]string, error) {
	if env == nil || env.Host == nil || restoreRevision != "" {
		return nil, nil
	}
	deliveries, err := undeclaredDeliveries(repo, gitopsPath, claim)
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
