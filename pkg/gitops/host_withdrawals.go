package gitops

// --- Withdrawing what the composition dropped, carried by the host module ---
//
// A binding is withdrawn only by a generation with `removed: true`
// (publish_delivery.go), and a module's own publish writes one for a binding
// it stops declaring. A module removed from the composition is published by
// nobody: its delivered tree is removed (cell_removals.go), and its namespace
// and delivery account leave the cell with it, so nothing was left to tell
// the host, which kept offering the last generation it applied.
//
// The host module — the one hosting the delivery API, which carries the
// environment's module envelope — carries the composition, and the host
// accepts a withdrawal from its delivery account as well as from the
// withdrawn module's own. So the host module's publish withdraws every
// binding a module the composition no longer declares last delivered to the
// environment: its identity alone, `removed: true`, at the next generation,
// in the host module's own overlay, signed by the publish's custody exactly
// like a live manifest and POSTed by the host's own delivery Job beside its
// live manifests. The removed module's tree is still deleted outright, so the
// cell needs no exception for it.
//
// Once delivered, the host module's base branch holds the withdrawal, and
// settlement carries it forward verbatim like any other (settledManifestSet):
// it is never written twice, and re-delivered it is the same generation and
// the same bytes, which the host answers as a replay (core's Admit).
//
// What a dropped module last delivered is read from the base branch while
// its tree is still there — the publication removing it is this one — and
// otherwise from the base branch's history: the last commit that held the
// tree's overlay for the environment. A binding its own tree already withdrew
// is not withdrawn again, since that withdrawal was its module's to deliver.

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// composedOutManifests answers, keyed by binding, the last manifest each
// module the composition no longer declares delivered to environment and did
// not withdraw: read from the base branch for the trees this publication
// removes (removing, repository-relative), and from history for the trees an
// earlier publication removed. A binding a live tree on the base branch
// delivers, or two dropped modules both delivered, is refused by name.
func composedOutManifests(ctx context.Context, repo, gitopsPath, baseBranch, environment string, removing []string, claim *compositionClaim) (map[string]deliveredManifest, error) {
	ref := "refs/remotes/origin/" + baseBranch
	modulesPath := deliveryModulesPath(gitopsPath)
	overlay := moduleManifestOverlay(environment)
	// Each dropped tree, and whether the base branch still holds it.
	trees := map[string]bool{}
	for _, tree := range removing {
		trees[tree] = true
	}
	gone, err := treesRemovedFromHistory(ctx, repo, ref, modulesPath, overlay)
	if err != nil {
		return nil, err
	}
	for _, module := range gone {
		tree := deliveryModulePath(gitopsPath, module)
		if _, seen := trees[tree]; seen || !claim.undeclared(module) || gitHasPath(ctx, repo, ref, tree) {
			// A tree the base branch still holds is the composition claim's
			// to judge (undeclaredDeliveries), never history's.
			continue
		}
		trees[tree] = false
	}
	names := make([]string, 0, len(trees))
	for tree := range trees {
		names = append(names, tree)
	}
	sort.Strings(names)
	withdraw := map[string]deliveredManifest{}
	delivered := map[string]string{}
	for _, tree := range names {
		last, lastErr := lastDeliveredManifests(ctx, repo, ref, baseBranch, tree+"/"+overlay, trees[tree])
		if lastErr != nil {
			return nil, lastErr
		}
		for binding, entry := range last {
			if entry.manifest.Removed {
				continue
			}
			if other, held := delivered[binding]; held {
				return nil, fmt.Errorf("the delivered trees %s and %s both delivered binding %s to %s and the composition declares neither module, so which one the host withdraws cannot be told", other, tree, binding, environment)
			}
			delivered[binding] = tree
			withdraw[binding] = deliveredManifest{manifest: entry.manifest}
		}
	}
	if len(withdraw) == 0 {
		return nil, nil
	}
	if err = refuseLiveWithdrawals(ctx, repo, ref, baseBranch, modulesPath, overlay, trees, delivered); err != nil {
		return nil, err
	}
	return withdraw, nil
}

// treesRemovedFromHistory names the module trees under modulesPath whose
// overlay files the base branch's history deleted: every module that once
// delivered to the overlay's environment and lost its delivery since. A
// tree moved under another name is a deletion here too, never a rename.
func treesRemovedFromHistory(ctx context.Context, repo, ref, modulesPath, overlay string) ([]string, error) {
	listing, err := gitCommand(ctx, repo, "log", "--no-renames", "--diff-filter=D", "--format=", "--name-only", ref, "--",
		":(glob)"+modulesPath+"/*/"+overlay+"/*")
	if err != nil {
		return nil, fmt.Errorf("read which module trees the delivery history removed from %s: %w", modulesPath, err)
	}
	seen := map[string]bool{}
	var modules []string
	for _, line := range strings.Split(listing, "\n") {
		rest, under := strings.CutPrefix(strings.TrimSpace(line), modulesPath+"/")
		module, _, _ := strings.Cut(rest, "/")
		if !under || module == "" || seen[module] {
			continue
		}
		seen[module] = true
		modules = append(modules, module)
	}
	sort.Strings(modules)
	return modules, nil
}

// lastDeliveredManifests reads what overlayPath last delivered: on the base
// branch when it holds it, otherwise at the last commit of its history that
// did. A tree the base branch still holds that never delivered there
// delivered nothing; a tree only history knows must have, so finding no
// commit is refused by name.
func lastDeliveredManifests(ctx context.Context, repo, ref, baseBranch, overlayPath string, onBase bool) (map[string]deliveredManifest, error) {
	revision, where := ref, baseBranch
	if !gitHasPath(ctx, repo, ref, overlayPath) {
		held, err := lastRevisionHolding(ctx, repo, ref, overlayPath)
		if err != nil {
			return nil, err
		}
		if held == "" {
			if onBase {
				return nil, nil
			}
			return nil, fmt.Errorf("the history of %s removed %s and holds no commit that delivered it, so the generations its module last delivered — which the host module's publish withdraws — cannot be read", baseBranch, overlayPath)
		}
		revision, where = held, baseBranch+" at "+held
	}
	files, err := overlayFilesAt(ctx, repo, revision, where, overlayPath)
	if err != nil {
		return nil, err
	}
	return deliveredManifestsAt(ctx, repo, revision, where, overlayPath, files)
}

// lastRevisionHolding is the last commit reachable from ref that holds path,
// "" when none does: the parent of the commit that last changed it, when ref
// no longer holds it.
func lastRevisionHolding(ctx context.Context, repo, ref, path string) (string, error) {
	changed, err := gitCommand(ctx, repo, "log", "-1", "--format=%H", ref, "--", path)
	if err != nil {
		return "", fmt.Errorf("read the history of %s: %w", path, err)
	}
	if changed == "" {
		return "", nil
	}
	if gitHasPath(ctx, repo, changed, path) {
		return changed, nil
	}
	parents, err := gitCommand(ctx, repo, "rev-list", "--parents", "-n", "1", changed)
	if err != nil {
		return "", fmt.Errorf("read the parents of %s: %w", changed, err)
	}
	for _, parent := range strings.Fields(parents)[1:] {
		if gitHasPath(ctx, repo, parent, path) {
			return parent, nil
		}
	}
	return "", nil
}

// refuseLiveWithdrawals refuses to withdraw a binding a tree the base branch
// still delivers holds live: the binding is that module's now, and only its
// own publish settles it.
func refuseLiveWithdrawals(ctx context.Context, repo, ref, baseBranch, modulesPath, overlay string, dropped map[string]bool, withdraw map[string]string) error {
	if !gitHasPath(ctx, repo, ref, modulesPath) {
		return nil
	}
	listing, err := gitCommand(ctx, repo, "ls-tree", "-d", "--name-only", ref+":"+modulesPath)
	if err != nil {
		return fmt.Errorf("list the module trees %s delivers: %w", baseBranch, err)
	}
	for _, module := range strings.Fields(listing) {
		tree := modulesPath + "/" + module
		overlayPath := tree + "/" + overlay
		if _, isDropped := dropped[tree]; isDropped || !gitHasPath(ctx, repo, ref, overlayPath) {
			continue
		}
		files, err := overlayFilesAt(ctx, repo, ref, baseBranch, overlayPath)
		if err != nil {
			return err
		}
		live, err := deliveredManifestsAt(ctx, repo, ref, baseBranch, overlayPath, files)
		if err != nil {
			return err
		}
		for binding, entry := range live {
			if from, held := withdraw[binding]; held && !entry.manifest.Removed {
				return fmt.Errorf("binding %s, which %s delivered before the composition dropped its module, is delivered live by %s on %s; the host module's publish will not withdraw a binding another module holds — give that instance a new name, which gives it a new binding", binding, from, tree, baseBranch)
			}
		}
	}
	return nil
}

// carryCompositionWithdrawals enters, for the publish of the module hosting
// the delivery API, the last manifest of each binding a dropped module
// delivered (opts.Withdraw) into prior, as though the host module's tree had
// delivered it: settlement then withdraws it at the next generation exactly
// as it withdraws a binding the host module stopped declaring — held to the
// same domain, host and admission, signed, written into the host module's
// overlay and POSTed by its Job. A binding the host module's base already
// carries a withdrawal of is left to it, carried forward and never written
// twice.
func carryCompositionWithdrawals(prior, rendered map[string]deliveredManifest, inventory *Inventory, opts *deliveryPublishOptions) error {
	if !inventory.HostsDelivery || opts.Target == nil {
		return nil
	}
	for _, binding := range sortedBindings(opts.Withdraw) {
		last := opts.Withdraw[binding]
		_, declared := rendered[binding]
		carried, delivered := prior[binding]
		if declared || delivered && !carried.manifest.Removed {
			return fmt.Errorf("module %s delivers binding %s, which module %s delivered before the composition dropped it; a new instance needs a new name, which gives it a new binding",
				inventory.Module, binding, last.manifest.Alias)
		}
		if !delivered {
			prior[binding] = last
		}
	}
	return nil
}

// withdrawnByHost describes, for the plan, each withdrawal the delivery
// record carries of a binding a dropped module delivered.
func withdrawnByHost(delivery *InventoryDelivery, withdraw map[string]deliveredManifest) []string {
	if delivery == nil {
		return nil
	}
	var described []string
	for _, document := range delivery.Documents {
		last, dropped := withdraw[document.ID]
		if document.Kind != deliveredManifestKind || !document.Removed || !dropped {
			continue
		}
		described = append(described, fmt.Sprintf("%s (module %s) at generation %d", document.ID, last.manifest.Alias, document.Generation))
	}
	return described
}
