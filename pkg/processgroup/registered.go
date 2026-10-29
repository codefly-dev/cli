package processgroup

import (
	"context"
	"errors"
	"fmt"

	runners "github.com/codefly-dev/core/runners/base"
)

// PreviewManagedProcessGroups lists live, authenticated, in-scope registrations
// without signaling or removing records. Refusals match managed stop admission:
// a group whose ownership cannot be decided is left out and named in the error,
// with how to recover it, and every other group is still listed.
func PreviewManagedProcessGroups(ctx context.Context, scope Scope) ([]int, error) {
	groups, err := runners.ListProcessGroups(ctx)
	if err != nil {
		return nil, err
	}
	var selected []int
	var kept []error
	for _, group := range groups {
		if !group.Alive() {
			continue
		}
		ownership, err := group.InspectOwnership(ctx)
		if err != nil {
			kept = append(kept, undecided(group.PGID(), err))
			continue
		}
		if !ownership.Authenticated {
			kept = append(kept, undecided(group.PGID(), errUnproven))
			continue
		}
		if registeredGroupInScope(group, ownership, scope) {
			selected = append(selected, group.PGID())
		}
	}
	return selected, errors.Join(kept...)
}

// errUnproven: the group's members do not prove they belong to its record.
var errUnproven = errors.New("live group ownership is unproven")

// undecided is a live registered group Core cannot decide is the one its
// record describes (an unproven owner, or a boot identity an older writer
// recorded differently). It is kept without signaling; the error names it and
// the supported way to recover it, so one such group never hides the others.
func undecided(pgid int, err error) error {
	return fmt.Errorf("inspect registered group %d: %w; kept without signaling, recover it with `codefly recover process-group %d --preview`", pgid, err, pgid)
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
		return CleanupFailed, false, undecided(group.PGID(), err)
	}
	if !ownership.Authenticated {
		return CleanupFailed, false, undecided(group.PGID(), errUnproven)
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
