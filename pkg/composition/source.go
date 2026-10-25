package composition

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/resources"
)

// ServiceUsesSource distinguishes a developer checkout or materialized clone
// from a published package. Core resolves both materializations to paths; the
// existing materialization receipts preserve how those paths were selected.
// This only reads provenance: it never pulls or changes a resolution.
func ServiceUsesSource(ctx context.Context, workspace *resources.Workspace, module, service string) (bool, error) {
	state, err := loadMaterializationState(ctx, workspace)
	if err != nil {
		return false, err
	}
	for _, ref := range workspace.Modules {
		if ref.Name != module {
			continue
		}
		var directive *resources.ModuleResolveDirective
		if state.overlay != nil {
			directive = state.overlay.Resolve[module]
		}
		receipt := state.receipts[module]
		mode := ResolutionModeFor(ref, directive, receipt, state.declared[module])
		if directive != nil {
			if override := directive.Services[service]; override != nil {
				managed := serviceManaged(override, state.receipts[serviceReceiptKey(module, service)].ResolvedPath(), state.roots.owned...)
				if managed && !serviceReceiptAnswers(ref, override, state.receipts[serviceReceiptKey(module, service)], mode) {
					return false, fmt.Errorf("service %s/%s materialization is unresolved or stale; resolve the composition before checking source freshness", module, service)
				}
				return !managed || mode.Unverified(), nil
			}
		}
		managed := pinnedManaged(ref, directive, receipt.ResolvedPath(), state.roots.owned...)
		if managed && !moduleReceiptAnswers(ref, directive, receipt, mode) {
			return false, fmt.Errorf("module %s materialization is unresolved or stale; resolve the composition before checking source freshness", module)
		}
		return !managed || mode.Unverified(), nil
	}
	return false, fmt.Errorf("module %s is not composed in workspace %s", module, workspace.Name)
}

// These are the same request/receipt checks used by the materializer's
// answered-state gate. A desired mode alone cannot describe selected bytes.
func moduleReceiptAnswers(ref *resources.ModuleReference, directive *resources.ModuleResolveDirective, receipt *ResolutionReceipt, mode ResolutionMode) bool {
	return directive != nil && directive.Path != "" && directive.Path == receipt.ResolvedPath() && receipt.Answers(ref, mode)
}

func serviceReceiptAnswers(ref *resources.ModuleReference, directive *resources.ServiceResolveDirective, receipt *ResolutionReceipt, mode ResolutionMode) bool {
	// A service's requested version lives only in its receipt after materialization;
	// the module identity and mode still have independent declarations to compare.
	return directive != nil && directive.Version == "" && directive.Path != "" && directive.Path == receipt.ResolvedPath() && receipt != nil && receipt.Source == ref.Source && receipt.Module == ref.Module && receipt.Mode == mode
}
