package composition

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/codefly-dev/core/resources"
)

// SelectedModuleDir is the directory a composed module resolves to right now,
// judged under the owner rules the materializer itself uses, and nothing else.
//
// It never pulls, materializes or reloads: a caller that reads bytes out of a
// composed module reads what this invocation already selected, so a reference
// the workspace cannot load as a local checkout is refused rather than fetched
// behind the read (ResolveComposedModuleDir is the materializing half, for
// callers whose job is to make the tree exist).
//
// Provenance decides how hard that refusal is. A path the CLI materialized —
// pinnedManaged, the same predicate the materializer gates its own writes on —
// must have a receipt that answers the request this workspace makes now, or
// the bytes under it are from some other resolution and the selection is
// stale. An explicit `path:`, an overlay path or a worktree legitimately has
// no receipt at all: it is the developer's own checkout, selected by them, and
// its bytes are local development evidence rather than release authenticity.
// Returning verified=false for those is the whole point — a caller that needs
// to say where bytes came from must not call a worktree a release.
func SelectedModuleDir(ctx context.Context, workspace *resources.Workspace, module string) (dir string, verified bool, err error) {
	var ref *resources.ModuleReference
	for _, candidate := range workspace.Modules {
		if candidate.Name == module {
			ref = candidate
			break
		}
	}
	if ref == nil {
		return "", false, fmt.Errorf("module %s is not composed in workspace %s", module, workspace.Name)
	}
	state, err := loadMaterializationState(ctx, workspace)
	if err != nil {
		return "", false, err
	}
	var directive *resources.ModuleResolveDirective
	if state.overlay != nil {
		directive = state.overlay.Resolve[module]
	}
	receipt := state.receipts[module]
	mode := ResolutionModeFor(ref, directive, receipt, state.declared[module])
	resolution, err := workspace.ResolveModule(ctx, ref)
	if err != nil {
		return "", false, err
	}
	if resolution.Kind == resources.ResolutionPinned {
		return "", false, fmt.Errorf("module %s is not materialized on this machine; resolve the composition before reading its tree", module)
	}
	if pinnedManaged(ref, directive, receipt.ResolvedPath(), state.roots.owned...) {
		if !moduleReceiptAnswers(ref, directive, receipt, mode) {
			return "", false, fmt.Errorf("module %s materialization is unresolved or stale; resolve the composition before reading its tree", module)
		}
		// Two sources had to agree to get here, and they are read at different
		// moments: the overlay and the receipts come off disk NOW, while
		// workspace.ResolveModule answers from the overlay snapshot the
		// workspace was LOADED with. A selection changed in between — a
		// re-materialization, an edited overlay — makes the receipt describe a
		// path the resolution no longer names, and the bytes would then be read
		// from one selection while attributed to another's receipt. Require the
		// resolved directory to be the receipt's path, or under it where the
		// reference names a module subpath, and refuse the divergence rather
		// than pick a side.
		if !sameSelectedDir(resolution.Dir, receipt.ResolvedPath()) {
			return "", false, fmt.Errorf("module %s resolves to a different directory than its materialization receipt records; resolve the composition again before reading its tree", module)
		}
		return resolution.Dir, !mode.Unverified(), nil
	}
	return resolution.Dir, false, nil
}

// sameSelectedDir compares the resolved directory with the receipt's, and
// requires them to be the SAME directory — not one under the other.
//
// The materializer records the module directory itself, with ref.Module's
// subpath already joined on (updateReceipt stores the resolved module dir, not
// the checkout root), so the receipt and the resolution are two spellings of
// one directory and nothing legitimately nests. A prefix test would pass a
// stale selection that happens to sit beneath the current one — exactly the
// case the reviewer's A-nested-under-B repro covers — so only normalized
// equality can tell "the same selection" from "a different one inside it".
func sameSelectedDir(dir, selected string) bool {
	if dir == "" || selected == "" {
		return false
	}
	return filepath.Clean(dir) == filepath.Clean(selected)
}
