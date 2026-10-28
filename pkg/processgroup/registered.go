package processgroup

import (
	"context"
	"errors"
	"fmt"

	runners "github.com/codefly-dev/core/runners/base"
)

// PreviewManagedProcessGroups lists live, authenticated, in-scope registrations
// without signaling or removing records. Refusals match managed stop admission.
func PreviewManagedProcessGroups(ctx context.Context, scope Scope) ([]int, error) {
	groups, err := runners.ListProcessGroups(ctx)
	if err != nil {
		return nil, err
	}
	var selected []int
	for _, group := range groups {
		if !group.Alive() {
			continue
		}
		ownership, err := group.InspectOwnership(ctx)
		if err != nil {
			return nil, fmt.Errorf("inspect registered group %d: %w", group.PGID(), err)
		}
		if !ownership.Authenticated {
			return nil, fmt.Errorf("group %d ownership is unproven", group.PGID())
		}
		if registeredGroupInScope(group, ownership, scope) {
			selected = append(selected, group.PGID())
		}
	}
	return selected, nil
}

// Core owns supported registry discovery, authentication and identity-bound
// signals. CLI supplies cleanup policy and evidence; the separate root scanner
// remains solely for historical line/JSON records outside Core's namespaces.
func reapRegisteredProcessGroups(ctx context.Context, policy reapPolicy) (CleanupEvidence, error) {
	evidence := CleanupEvidence{Outcome: CleanupClean}
	groups, err := runners.ListProcessGroups(ctx)
	if err != nil {
		evidence.Outcome = CleanupFailed
		return evidence, err
	}
	var failures []error
	for _, group := range groups {
		outcome, outside, err := reconcileRegisteredGroup(ctx, group, policy)
		if outside {
			evidence.OutOfScope = append(evidence.OutOfScope, group.PGID())
			continue
		}
		if err != nil {
			outcome = CleanupFailed
			failures = append(failures, fmt.Errorf("reconcile registered group %d: %w", group.PGID(), err))
		}
		// An owner handle has an identity, not a consumer-owned record path.
		evidence.record(fmt.Sprintf("registered-group:%d", group.PGID()), group.PGID(), outcome)
	}
	return evidence, errors.Join(failures...)
}

func reconcileRegisteredGroup(ctx context.Context, group *runners.TrackedProcessGroup, policy reapPolicy) (CleanupOutcome, bool, error) {
	if err := ctx.Err(); err != nil {
		return CleanupFailed, false, err
	}
	if !group.Alive() {
		return CleanupClean, false, group.RemoveIfDeadContext(ctx)
	}
	ownership, err := group.InspectOwnership(ctx)
	if err != nil {
		return CleanupFailed, false, err
	}
	if !ownership.Authenticated {
		return CleanupFailed, false, fmt.Errorf("live group ownership is unproven; registration retained without signaling")
	}
	if !registeredGroupInScope(group, ownership, policy.scope) {
		return CleanupClean, true, nil
	}
	if ownership.OwnerAlive && !policy.stopManaged {
		return CleanupClean, false, nil
	}
	if err := group.Terminate(ctx); err != nil {
		return CleanupFailed, false, err
	}
	if err := group.RemoveIfDeadContext(ctx); err != nil {
		return CleanupFailed, false, err
	}
	if !ownership.OwnerAlive {
		return CleanupRecoveredOrphan, false, nil
	}
	return CleanupClean, false, nil
}

func registeredGroupInScope(group *runners.TrackedProcessGroup, ownership *runners.ProcessGroupOwnership, scope Scope) bool {
	if scope.All() {
		return true
	}
	if ownership.LaunchWorkspace != "" {
		return scope.Includes(ownership.LaunchWorkspace)
	}
	// Older registrations have no persisted workspace. Preserve their existing
	// environment/cwd attribution only after Core has authenticated the group.
	return scope.includesProcessGroup(group.PGID())
}
